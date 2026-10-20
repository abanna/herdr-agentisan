package settings

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
)

// telemetryEndpointPath is the OTLP/HTTP collector the daemon sends its
// workflow events to (ADR-001 D10, A21).
const telemetryEndpointPath = "telemetry.endpoint"

// telemetrySchema is [telemetry]. Like [spaces] it belongs to the shared file
// and --set only: there is one daemon per herdr server, not one per project.
// The token for a collector across the network arrives with TLS (A21); until
// then any other key is unknown.
var telemetrySchema = &node{kind: kindTable, fields: map[string]*node{
	"endpoint": leafOf(kindString),
}}

// endpointHost is the alphabet of the endpoint's host: a name, an IPv4
// address, or an IPv6 one (unbracketed by SplitHostPort, a zone included).
// Anything else, a slash or an @ above all, would bend the URL the exporter
// builds around it.
var endpointHost = regexp.MustCompile(`^[A-Za-z0-9._%:-]+$`)

// endpointHint is what an invalid endpoint's error says instead of echoing
// the value, which may carry credentials (user:password@host).
const endpointHint = "not host:port, such as localhost:4318"

// checkTelemetry checks telemetry.endpoint is host:port, as
// HERDR_AGENTISAN_OTLP_ENDPOINT is: the exporter prefixes http:// and
// appends /v1/logs. Empty is allowed and means nothing is exported.
func (m merged) checkTelemetry() error {
	v, ok := m.get(telemetryEndpointPath)
	if !ok {
		return nil
	}
	s, _ := v.(string)
	if s == "" || validEndpoint(s) {
		return nil
	}
	return m.invalidf(telemetryEndpointPath, endpointHint)
}

func validEndpoint(s string) bool {
	host, port, err := net.SplitHostPort(s)
	if err != nil || !endpointHost.MatchString(host) {
		return false
	}
	n, err := strconv.ParseUint(port, 10, 16)
	return err == nil && n > 0
}

// TelemetryEndpoint is telemetry.endpoint as resolved, which Load has
// checked; "" is none.
func (r Resolved) TelemetryEndpoint() string {
	v, _ := lookup(r.Values, telemetryEndpointPath)
	s, _ := v.(string)
	return s
}

// TelemetryEndpoint is the collector the daemon sends its workflow events
// to, read once when it starts: override when it is set (the daemon passes
// HERDR_AGENTISAN_OTLP_ENDPOINT), without reading the file at all;
// otherwise telemetry.endpoint from the shared file in configDir. "" is none,
// and nothing is exported. No configDir, or no file in it, is none and not
// an error. The read selects no project, so it touches no repo and reads no
// environment variable.
//
// The override must be host:port as the key must, and is refused with
// ErrInvalid otherwise: the daemon logs the endpoint it uses, so one
// carrying user:password@ never reaches the log.
func TelemetryEndpoint(configDir, override string) (string, error) {
	if override != "" {
		if !validEndpoint(override) {
			return "", fmt.Errorf("the endpoint override (HERDR_AGENTISAN_OTLP_ENDPOINT): %w: %s", ErrInvalid, endpointHint)
		}
		return override, nil
	}
	if configDir == "" {
		return "", nil
	}
	res, err := Load(LoadOptions{ConfigDir: configDir, LookupEnv: func(string) (string, bool) { return "", false }})
	if err != nil {
		return "", err
	}
	return res.TelemetryEndpoint(), nil
}
