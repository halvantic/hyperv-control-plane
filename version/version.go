// Package version holds the single Ballast release version string. Both the
// agent and centre import this so the centre can detect when a host is running
// a stale agent binary and surface an "update required" indicator.
package version

const Version = "0.4.42-slice"
