package tenderduty

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

func serveFibreHealth(t *testing.T, cert tls.Certificate, registered bool) (string, *health.Server, *atomic.Bool, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	failing := new(atomic.Bool)
	server := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13})),
		grpc.UnaryInterceptor(func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
			if failing.Load() {
				return nil, status.Error(codes.Unavailable, "health check failed")
			}
			return handler(ctx, req)
		}),
	)
	h := health.NewServer()
	if registered {
		healthpb.RegisterHealthServer(server, h)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return listener.Addr().String(), h, failing, server.Stop
}

func TestFibreHealth(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := fibreCertificateFor(t, priv, "mocha-5", true)
	host, h, failing, _ := serveFibreHealth(t, cert, true)
	peer, err := fibreCertificate(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyFibreIdentity(peer, pub, "mocha-5", time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, serving := range []healthpb.HealthCheckResponse_ServingStatus{
		healthpb.HealthCheckResponse_SERVING,
		healthpb.HealthCheckResponse_NOT_SERVING,
		healthpb.HealthCheckResponse_UNKNOWN,
	} {
		h.SetServingStatus("", serving)
		known, ready, err := fibreHealth(context.Background(), host, pub, "mocha-5")
		if err != nil || !known || ready != (serving == healthpb.HealthCheckResponse_SERVING) {
			t.Fatalf("%s: known %v, ready %v, err %v", serving, known, ready, err)
		}
	}
	failing.Store(true)
	if _, _, err := fibreHealth(context.Background(), host, pub, "mocha-5"); status.Code(err) != codes.Unavailable {
		t.Fatalf("health failure: %v", err)
	}
	old, _, _, _ := serveFibreHealth(t, cert, false)
	known, ready, err := fibreHealth(context.Background(), old, pub, "mocha-5")
	if err != nil || known || ready {
		t.Fatalf("unimplemented: known %v, ready %v, err %v", known, ready, err)
	}
	// A TLS-only listener proves reachability but reports no readiness.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	known, ready, err = fibreHealth(ctx, serveFibre(t, cert), pub, "mocha-5")
	if err == nil || known || ready {
		t.Fatalf("TLS-only: known %v, ready %v, err %v", known, ready, err)
	}
}

func TestFibreHealthIdentity(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, otherPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	host, _, _, _ := serveFibreHealth(t, fibreCertificateFor(t, otherPriv, "mocha-5", true), true)
	known, ready, err := fibreHealth(context.Background(), host, pub, "mocha-5")
	if err == nil || !strings.Contains(err.Error(), "not signed by this validator") || known || ready {
		t.Fatalf("another key: known %v, ready %v, err %v", known, ready, err)
	}
	known, ready, err = fibreHealth(context.Background(), host, nil, "mocha-5")
	if err != nil || !known || !ready {
		t.Fatalf("no key: known %v, ready %v, err %v", known, ready, err)
	}
}

func TestRefreshFibreHealthIdentityFailure(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, otherPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	host, h, _, _ := serveFibreHealth(t, fibreCertificateFor(t, otherPriv, "mocha-5", true), true)
	for _, ready := range []bool{false, true} {
		t.Run(map[bool]string{false: "not ready", true: "ready"}[ready], func(t *testing.T) {
			cc := fibreChain()
			cc.ChainId, cc.valInfo.ConsKey = "mocha-5", pub
			since := time.Time{}
			if ready {
				h.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
			} else {
				since = time.Now().Add(-10 * time.Minute)
				h.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
			}
			cc.fibre = fibreState{Registered: true, Host: host, HealthKnown: true, Ready: ready, NotReadySince: since}
			queries := 0
			cc.refreshFibre(context.Background(), fibreNode(t, fibreRegistration(host), &queries))
			if s := cc.fibre; !s.Reachable || !s.KeyKnown || s.IdentityOK || !s.HealthKnown || s.Ready != ready || !s.NotReadySince.Equal(since) {
				t.Fatalf("identity failure changed the last health result: %+v", s)
			}
		})
	}
}

func TestRefreshFibreHealth(t *testing.T) {
	c := auditConfig(t)
	c.NodeDownMin = 5
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	host, h, failing, stop := serveFibreHealth(t, fibreCertificateFor(t, priv, "mocha-5", true), true)
	cc := fibreChain()
	cc.name, cc.ChainId, cc.valInfo.ConsKey = "Chain", "mocha-5", pub
	cc.Alerts.FibreAlerts, cc.Alerts.FibreSeverity = true, "warning"
	c.Chains[cc.name] = cc
	queries := 0
	client := fibreNode(t, fibreRegistration(host), &queries)
	refresh := func() { cc.refreshFibre(context.Background(), client) }
	message := fibreNotReadyMessage(host, cc.ChainId)
	refresh()
	cc.fibreAlarms(cc.valInfo, false)
	if s := cc.fibre; !s.HealthKnown || !s.Ready || !s.NotReadySince.IsZero() || alarmIsActive(cc.name, message) {
		t.Fatalf("serving: %+v", s)
	}
	h.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
	refresh()
	cc.fibreAlarms(cc.valInfo, false)
	if s := cc.fibre; !s.HealthKnown || s.Ready || s.NotReadySince.IsZero() || alarmIsActive(cc.name, message) {
		t.Fatalf("new failure: %+v", s)
	}
	cc.fibre.NotReadySince = time.Now().Add(-10 * time.Minute)
	since := cc.fibre.NotReadySince
	refresh()
	cc.fibreAlarms(cc.valInfo, false)
	if !cc.fibre.NotReadySince.Equal(since) || !alarmIsActive(cc.name, message) {
		t.Fatalf("lasting failure: %+v", cc.fibre)
	}
	raised := 0
	for len(c.alertChan) > 0 {
		if alert := <-c.alertChan; alert.message == message && !alert.resolved {
			raised++
		}
	}
	if raised != 1 {
		t.Fatalf("not-ready alarm raised %d times", raised)
	}
	cc.fibreAlarms(cc.valInfo, false)
	if len(c.alertChan) != 0 {
		t.Fatal("unchanged readiness raised another alarm")
	}
	failing.Store(true)
	refresh()
	cc.fibreAlarms(cc.valInfo, false)
	if s := cc.fibre; !s.Reachable || !s.HealthKnown || s.Ready || !s.NotReadySince.Equal(since) || !alarmIsActive(cc.name, message) {
		t.Fatalf("health error changed the last result: %+v", s)
	}
	failing.Store(false)
	h.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	refresh()
	cc.fibreAlarms(cc.valInfo, false)
	if s := cc.fibre; !s.Ready || !s.NotReadySince.IsZero() || alarmIsActive(cc.name, message) {
		t.Fatalf("recovered: %+v", s)
	}
	r := cc.fibreResolution(message, cc.fibre, "Validator")
	if r.icon != "✅" || r.label != "Fibre ready" || r.text != "Fibre at "+host+" is ready again on mocha-5" {
		t.Fatalf("resolution: %+v", r)
	}
	failing.Store(true)
	refresh()
	if !cc.fibre.HealthKnown || !cc.fibre.Ready || !cc.fibre.NotReadySince.IsZero() {
		t.Fatalf("health error cleared serving: %+v", cc.fibre)
	}
	failing.Store(false)
	h.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
	refresh()
	cc.fibre.NotReadySince = since
	cc.fibreAlarms(cc.valInfo, false)
	stop()
	refresh()
	cc.fibreAlarms(cc.valInfo, false)
	if s := cc.fibre; s.Reachable || !s.HealthKnown || s.Ready || !s.NotReadySince.Equal(since) || !alarmIsActive(cc.name, message) {
		t.Fatalf("downtime lost readiness: %+v", s)
	}
	other, _, otherFailing, _ := serveFibreHealth(t, fibreCertificateFor(t, priv, "mocha-5", true), true)
	otherFailing.Store(true)
	cc.refreshFibre(context.Background(), fibreNode(t, fibreRegistration(other), &queries))
	if s := cc.fibre; s.HealthKnown || s.Ready || !s.NotReadySince.IsZero() {
		t.Fatalf("new host inherited readiness: %+v", s)
	}
}

func TestFibreReadinessStatus(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name, ready, text string
		known, serving    bool
	}{
		{"serving", "yes", "🟢 yes", true, true},
		{"not serving", "no", "🟡 no, for 5m", true, false},
		{"older server", "unknown", "⚪ not reported (Fibre before v10.5)", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &fibreReport{Now: now, State: fibreState{Registered: true, Reachable: true, HealthKnown: tc.known, Ready: tc.serving}}
			if tc.known && !tc.serving {
				r.State.NotReadySince = now.Add(-5 * time.Minute)
			}
			got := strings.Join(r.telegramLines(), "\n")
			if !strings.Contains(got, "🩺 <b>Ready:</b> "+tc.text) || strings.Index(got, "<b>Ready:</b>") < strings.Index(got, "<b>Identity:</b>") {
				t.Fatalf("Telegram: %s", got)
			}
			body, err := json.Marshal(r.dashboard())
			if err != nil || !strings.Contains(string(body), `"ready":"`+tc.ready+`"`) {
				t.Fatalf("JSON: %s, %v", body, err)
			}
			r.State.Reachable = false
			got = strings.Join(r.telegramLines(), "\n")
			if strings.Contains(got, "<b>Ready:</b>") != tc.known {
				t.Fatalf("downtime status: %s", got)
			}
		})
	}
}

func TestFibreWithoutHealth(t *testing.T) {
	for _, plain := range []bool{false, true} {
		t.Run(map[bool]string{false: "unimplemented", true: "TLS only"}[plain], func(t *testing.T) {
			c := auditConfig(t)
			pub, priv, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			cert := fibreCertificateFor(t, priv, "mocha-5", true)
			var host string
			if plain {
				host = serveFibre(t, cert)
			} else {
				host, _, _, _ = serveFibreHealth(t, cert, false)
			}
			cc := fibreChain()
			cc.name, cc.ChainId, cc.valInfo.ConsKey = "Chain", "mocha-5", pub
			cc.Alerts.FibreAlerts, cc.Alerts.FibreSeverity = true, "warning"
			c.Chains[cc.name] = cc
			queries := 0
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			cc.refreshFibre(ctx, fibreNode(t, fibreRegistration(host), &queries))
			cc.fibreAlarms(cc.valInfo, false)
			if s := cc.fibre; !s.Reachable || !s.IdentityOK || s.HealthKnown || s.Ready || !s.NotReadySince.IsZero() {
				t.Fatalf("no health service: %+v", s)
			}
			if left := activeMessages(cc.name, fibreHostPrefix); len(left) != 0 {
				t.Fatalf("no health service raised alarms: %v", left)
			}
		})
	}
}

func TestFibreReadinessPrometheus(t *testing.T) {
	r := statusRig(t, false, nil)
	registry := prometheus.NewRegistry()
	registry.MustRegister(&fibreCollector{config: r.c})
	for _, tc := range []struct {
		known, ready bool
		want         float64
	}{{false, false, 0}, {true, false, 0}, {true, true, 1}} {
		r.cc.fibre.HealthKnown, r.cc.fibre.Ready = tc.known, tc.ready
		families, err := registry.Gather()
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, family := range families {
			if family.GetName() == "tenderduty_fibre_ready" {
				found = true
				if got := family.Metric[0].GetGauge().GetValue(); got != tc.want {
					t.Fatalf("ready gauge %v, want %v", got, tc.want)
				}
			}
		}
		if found != tc.known {
			t.Fatalf("ready gauge present %v, known %v", found, tc.known)
		}
	}
}

func TestFibreNotReadyResolution(t *testing.T) {
	cc := &ChainConfig{ChainId: "mocha-5"}
	message := fibreNotReadyMessage("old:7980", cc.ChainId)
	if got := fibreAlarmId(&ValInfo{Valcons: "cons"}, message); got != "consfibrenotreadyold:7980" {
		t.Fatalf("alarm id: %s", got)
	}
	for _, tc := range []struct {
		state fibreState
		label string
	}{
		{fibreState{}, "No longer registered"},
		{fibreState{Registered: true, Host: "new:7980"}, "Host changed"},
		{fibreState{Registered: true, Host: "old:7980", Ready: true, HealthKnown: true}, "Fibre ready"},
	} {
		if got := cc.fibreResolution(message, tc.state, "Validator"); got.label != tc.label {
			t.Fatalf("resolution: %+v, want %s", got, tc.label)
		}
	}
}
