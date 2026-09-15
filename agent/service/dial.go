package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"

	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// transportCreds chooses the gRPC transport security for the agent's connection
// to the centre. When tlsDir holds the CA plus this agent's client cert/key
// (provisioned at onboard), it connects over mutual TLS: the channel is
// encrypted and the centre binds the connection to this host's identity.
//
// tlsDir empty is the explicit, deliberate opt-out — the operator did not pass
// -tls-dir, so there is nothing to load and the agent connects insecure. That
// is the only case this returns an insecure option; every other failure to
// build the mTLS transport is fail-CLOSED (returns an error, no DialOption),
// because this channel carries desired state and secrets. It used to fall
// back to insecure with a logged warning whenever the cert material was
// missing or invalid — silently, from the operator's point of view, since
// nothing about a running agent looks different whether it is encrypted or
// not. A missing or unreadable file and a channel that has quietly dropped
// its only protection must not be the same outcome; refusing to start is the
// one way a broken -tls-dir setup cannot go unnoticed.
//
// TLS only governs talking to the centre; it never gates the agent enforcing
// its cached desired state, so refusing to start here does not touch
// reconciliation directly — but the agent cannot start at all without a
// transport, so a fail-closed misconfiguration does mean the agent will not
// run until -tls-dir is fixed (or explicitly cleared). That is the intended
// trade: a broken mTLS setup should be loud, not a silent downgrade.
func transportCreds(tlsDir string, log *slog.Logger) (grpc.DialOption, error) {
	if tlsDir == "" {
		return grpc.WithTransportCredentials(insecure.NewCredentials()), nil
	}
	caPEM, caErr := os.ReadFile(filepath.Join(tlsDir, "ca.pem"))
	cert, certErr := tls.LoadX509KeyPair(
		filepath.Join(tlsDir, "agent-cert.pem"),
		filepath.Join(tlsDir, "agent-key.pem"),
	)
	if caErr != nil || certErr != nil {
		log.Error("mTLS material missing or unreadable at -tls-dir; refusing to start rather than connecting insecure",
			"tlsDir", tlsDir, "caErr", caErr, "certErr", certErr)
		return nil, fmt.Errorf("mTLS material at -tls-dir %q is missing or unreadable (ca: %v, cert/key: %v) — "+
			"fix the files, re-provision the agent, or pass -tls-dir \"\" to explicitly run insecure", tlsDir, caErr, certErr)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		log.Error("invalid CA certificate at -tls-dir; refusing to start rather than connecting insecure", "tlsDir", tlsDir)
		return nil, fmt.Errorf("mTLS material at -tls-dir %q: ca.pem does not contain a valid certificate — "+
			"fix the file, re-provision the agent, or pass -tls-dir \"\" to explicitly run insecure", tlsDir)
	}
	log.Info("connecting to centre over mutual TLS", "tlsDir", tlsDir)
	return grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS12,
	})), nil
}
