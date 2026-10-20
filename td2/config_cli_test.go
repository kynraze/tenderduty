package tenderduty

import (
	"bytes"
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigCLISet(t *testing.T) {
	options, _, _ := chainCommandFixture(t)
	original := "# shared settings\nlisten_port: '8080' # dashboard\nalert_defaults:\n  upgrade_enabled: yes\n"
	if err := os.WriteFile(options.ConfigFile, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	options.reload = func(string) (int, error) { calls++; return 123, nil }
	var output bytes.Buffer
	run := func(args ...string) error {
		output.Reset()
		return ConfigCommand(context.Background(), options, args, strings.NewReader("stdin-secret\n"), &output)
	}
	if err := run("set", "LISTEN_PORT=9090"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(options.ConfigFile)
	if !strings.Contains(string(data), "# shared settings") || !strings.Contains(string(data), "# dashboard") || strings.Index(string(data), "listen_port") > strings.Index(string(data), "alert_defaults") || calls != 0 || !strings.Contains(output.String(), "restart tenderduty to apply listen_port") {
		t.Fatalf("data %s, output %s, calls %d", data, &output, calls)
	}
	if err := run("set", "reports.daily=09:00"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("reloads %d", calls)
	}
	data, _ = os.ReadFile(options.ConfigFile)
	if !strings.Contains(string(data), "reports:") || !strings.Contains(string(data), `daily: "09:00"`) {
		t.Fatalf("%s", data)
	}
	for _, tc := range []struct{ setting, want string }{
		{"listen_prt=7", "did you mean listen_port"},
		{"otel_metrics.t=abc", "did you mean otel_metrics.tls_cert or otel_metrics.tls_key or otel_metrics.token"},
		{"chains.X=foo", "tenderduty chain set"},
		{"servers.rpc.example.com=RPC", "edit servers in config.yml"},
		{"servers={}", "edit servers in config.yml"},
		{"reports.include_testnets=maybe", "yes or no"},
		{"otel_metrics.listen=broken", "configuration is invalid"},
		{"alert_defaults.telegram.min_severity=bad", "must be info"},
	} {
		before, _ := os.ReadFile(options.ConfigFile)
		err := run("set", tc.setting)
		after, _ := os.ReadFile(options.ConfigFile)
		if err == nil || !strings.Contains(err.Error(), tc.want) || !bytes.Equal(before, after) {
			t.Fatalf("%s: %v, changed %t", tc.setting, err, !bytes.Equal(before, after))
		}
	}
	if err := run("set", "telegram.api_key=-"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(options.ConfigFile)
	if !strings.Contains(string(data), "stdin-secret") || strings.Contains(output.String(), "stdin-secret") {
		t.Fatalf("%s / %s", data, &output)
	}
	if err := run("set", "otel_metrics.token=generate"); err != nil {
		t.Fatal(err)
	}
	_, token, _ := strings.Cut(output.String(), "copy it to Fibre/bridge now: ")
	token = strings.SplitN(token, "\n", 2)[0]
	decoded, err := hex.DecodeString(token)
	if err != nil || len(decoded) != 32 || strings.Count(output.String(), token) != 1 {
		t.Fatalf("token: %s", &output)
	}
	if err := run("show", "otel_metrics"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), token) || !strings.Contains(output.String(), "token: ***") || strings.Contains(output.String(), "telegram:") {
		t.Fatalf("%s", &output)
	}
}

func TestConfigCLISetListenHost(t *testing.T) {
	for _, listen := range []string{"127.0.0.1:8888", "[::1]:8888"} {
		t.Run(listen, func(t *testing.T) {
			options, _, _ := chainCommandFixture(t)
			original := "enable_dashboard: yes\nlisten_port: 8888 # dashboard\n"
			if err := os.WriteFile(options.ConfigFile, []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			if err := ConfigCommand(context.Background(), options, []string{"set", "listen_port=" + listen}, strings.NewReader(""), &output); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(options.ConfigFile)
			if err != nil {
				t.Fatal(err)
			}
			c := &Config{}
			if err := decodeConfig(data, c); err != nil || c.Listen != listen {
				t.Fatalf("saved listen %q: %v, data %s", c.Listen, err, data)
			}
			if !strings.Contains(string(data), "# dashboard") || !strings.Contains(output.String(), "restart tenderduty to apply listen_port") {
				t.Fatalf("data %s, output %s", data, &output)
			}
		})
	}
}

func TestConfigCLISetWithoutChains(t *testing.T) {
	dir := t.TempDir()
	password := ""
	options := ChainCommandOptions{ConfigFile: filepath.Join(dir, "config.yml"), Directory: filepath.Join(dir, "chains.d"), StateFile: filepath.Join(dir, "state.json"), Password: &password}
	if err := os.Mkdir(options.Directory, 0700); err != nil {
		t.Fatal(err)
	}
	original := "enable_dashboard: yes\nlisten_port: 8888\ntelegram:\n  enabled: no\n"
	if err := os.WriteFile(options.ConfigFile, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := ConfigCommand(context.Background(), options, []string{"set", "telegram.enabled=yes"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(options.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(before), "telegram:\n  enabled: yes\n") || !strings.Contains(output.String(), "telegram.enabled: no -> yes") {
		t.Fatalf("data %s, output %s", before, &output)
	}
	err = ConfigCommand(context.Background(), options, []string{"set", "listen_port=abc"}, strings.NewReader(""), &output)
	if err == nil || !strings.Contains(err.Error(), "dashboard port abc is invalid") {
		t.Fatalf("invalid dashboard port: %v", err)
	}
	after, err := os.ReadFile(options.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("invalid edit changed config: %s", after)
	}
}

func TestConfigShow(t *testing.T) {
	options, filename, original := chainCommandFixture(t)
	original = bytes.ReplaceAll(original, []byte("  fibre_enabled: false # Keep this comment\n"), nil)
	if err := os.WriteFile(filename, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(options.Directory, "Other.yml"), original, 0600); err != nil {
		t.Fatal(err)
	}
	config := "telegram:\n  api_key: do-not-show\notel_metrics:\n  token: hide-me\nalert_defaults:\n  fibre_enabled: yes\n"
	if err := os.WriteFile(options.ConfigFile, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := ConfigCommand(context.Background(), options, []string{"show"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{options.ConfigFile, "api_key: ***", "token: ***", "(restart to change)", "own value on:", "Celestia (critical)", "Change one:"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("missing %s: %s", want, &output)
		}
	}
	if strings.Contains(output.String(), "do-not-show") || strings.Contains(output.String(), "hide-me") {
		t.Fatalf("secret: %s", &output)
	}
	output.Reset()
	if err := ConfigCommand(context.Background(), options, []string{"show", "TeLeGrAm"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "otel_metrics:") {
		t.Fatalf("%s", &output)
	}
	if err := ConfigCommand(context.Background(), options, []string{"show", "bogus"}, strings.NewReader(""), &output); err == nil || !strings.Contains(err.Error(), "sections:") {
		t.Fatal(err)
	}
}

func TestConfigCLIRefusalsAndHelp(t *testing.T) {
	options, _, _ := chainCommandFixture(t)
	var output bytes.Buffer
	for _, args := range [][]string{nil, {"set", "-h"}, {"show", "-h"}} {
		if err := ConfigCommand(context.Background(), options, args, strings.NewReader(""), &output); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"set", "listen_port", "=", "9090"}, {"set", "listen_port=", "9090"}} {
		if err := ConfigCommand(context.Background(), options, args, strings.NewReader(""), &output); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	os.Remove(options.ConfigFile)
	if err := ConfigCommand(context.Background(), options, []string{"show"}, strings.NewReader(""), &output); err == nil || !strings.Contains(err.Error(), "create one first") {
		t.Fatal(err)
	}
	if err := os.WriteFile(options.ConfigFile+".asc", []byte("encrypted"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ConfigCommand(context.Background(), options, []string{"set", "listen_port=9090"}, strings.NewReader(""), &output); err == nil || !strings.Contains(err.Error(), "decrypt it (tenderduty -decrypt)") {
		t.Fatal(err)
	}
	options.ConfigFile = "https://example.invalid/config.yml.asc"
	if err := ConfigCommand(context.Background(), options, []string{"set", "listen_port=9090"}, strings.NewReader(""), &output); err == nil || !strings.Contains(err.Error(), "encrypted/remote") {
		t.Fatal(err)
	}
}

func TestConfigShowRelativeKeysAndEmptyValues(t *testing.T) {
	options, _, _ := chainCommandFixture(t)
	config := "telegram:\n  enabled: yes\n  api_key: secret\n  admins: []\notel_metrics:\n  tls_cert: ''\nalert_defaults:\n  telegram:\n    api_key: ''\n"
	if err := os.WriteFile(options.ConfigFile, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		section string
		want    []string
	}{
		{"telegram", []string{"  enabled: yes", "  api_key: ***", "  admins: -", "Change one: tenderduty config set telegram.enabled=no"}},
		{"otel_metrics", []string{"  tls_cert: -", "  token: -"}},
		{"alert_defaults", []string{"  telegram.api_key: -", "  telegram.enabled: -"}},
		{"general", []string{"general:", "Change one: tenderduty config set "}},
	} {
		var output bytes.Buffer
		if err := ConfigCommand(context.Background(), options, []string{"show", tc.section}, strings.NewReader(""), &output); err != nil {
			t.Fatal(err)
		}
		for _, want := range tc.want {
			if !strings.Contains(output.String(), want) {
				t.Fatalf("missing %q: %s", want, &output)
			}
		}
		if strings.Contains(output.String(), "  "+tc.section+".") || strings.Contains(output.String(), "secret") || strings.Contains(output.String(), `: ""`) || strings.Contains(output.String(), ": unset") {
			t.Fatal(&output)
		}
	}
}
