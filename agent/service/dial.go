package main

import (
	"crypto/tls"
	"crypto/x509"
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
// Otherwise it connects insecure, preserving backwards compatibility.
//
// TLS only governs talking to the centre; it never gates the agent enforcing its
// cached desired state, so a lapsed or missing certificate cannot wedge
// reconciliation — the agent runs autonomously until it is re-provisioned.
func transportCreds(tlsDir string, log *slog.Logger) grpc.DialOption {
	if tlsDir == "" {
		return grpc.WithTransportCredentials(insecure.NewCredentials())
	}
	caPEM, caErr := os.ReadFile(filepath.Join(tlsDir, "ca.pem"))
	cert, certErr := tls.LoadX509KeyPair(
		filepath.Join(tlsDir, "agent-cert.pem"),
		filepath.Join(tlsDir, "agent-key.pem"),
	)
	if caErr != nil || certErr != nil {
		log.Warn("mTLS material not available; connecting insecure",
			"tlsDir", tlsDir, "caErr", caErr, "certErr", certErr)
		return grpc.WithTransportCredentials(insecure.NewCredentials())
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		log.Warn("invalid CA certificate; connecting insecure", "tlsDir", tlsDir)
		return grpc.WithTransportCredentials(insecure.NewCredentials())
	}
	log.Info("connecting to centre over mutual TLS", "tlsDir", tlsDir)
	return grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS12,
	}))
}
