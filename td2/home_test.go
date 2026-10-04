package tenderduty

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResolvePaths(t *testing.T) {
	t.Chdir(t.TempDir())
	setSystemPaths(t, filepath.Join(t.TempDir(), "config.yml"), "chains.d", "state.json")
	home := filepath.Join("srv", "td")
	for name, tc := range map[string]struct {
		home, f, cc, state       string
		wantF, wantCC, wantState string
	}{
		"no home":              {"", "", "", "", "config.yml", "chains.d", ".tenderduty-state.json"},
		"home":                 {home, "", "", "", filepath.Join(home, "config.yml"), filepath.Join(home, "chains.d"), filepath.Join(home, ".tenderduty-state.json")},
		"explicit -f wins":     {home, "/etc/td.yml", "", "", "/etc/td.yml", filepath.Join(home, "chains.d"), filepath.Join(home, ".tenderduty-state.json")},
		"explicit all win":     {home, "/a.yml", "/b.d", "/c.json", "/a.yml", "/b.d", "/c.json"},
		"relative stays local": {home, "my.yml", "", "s.json", "my.yml", filepath.Join(home, "chains.d"), "s.json"},
		"no home, explicit":    {"", "x.yml", "", "", "x.yml", "chains.d", ".tenderduty-state.json"},
	} {
		f, cc, state := ResolvePaths(tc.home, tc.f, tc.cc, tc.state)
		if f != tc.wantF || cc != tc.wantCC || state != tc.wantState {
			t.Errorf("%s: got %q %q %q", name, f, cc, state)
		}
	}
}

func TestResolveHome(t *testing.T) {
	dir := t.TempDir()
	if got, err := ResolveHome(dir); err != nil || got != dir {
		t.Errorf("existing home: %q, %v", got, err)
	}
	if got, err := ResolveHome(""); err != nil || got != "" {
		t.Errorf("no home: %q, %v", got, err)
	}
	missing := filepath.Join(dir, "nope")
	if _, err := ResolveHome(missing); err == nil || err.Error() != "home directory "+missing+" does not exist" {
		t.Errorf("missing home: %v", err)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveHome(file); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("file as home: %v", err)
	}
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	if err := os.Mkdir(filepath.Join(dir, "td"), 0700); err != nil {
		t.Fatal(err)
	}
	if got, err := ResolveHome("~/td"); err != nil || got != filepath.Join(dir, "td") {
		t.Errorf("~/td: %q, %v", got, err)
	}
}

func TestChainCommandHonoursHome(t *testing.T) {
	options, _, _ := chainCommandFixture(t)
	home := filepath.Dir(options.ConfigFile)
	var output strings.Builder
	for _, args := range [][]string{{"list"}, {"list", "-home", home}} {
		given := ChainCommandOptions{Password: options.Password}
		if len(args) == 1 {
			given.Home = home
		}
		output.Reset()
		if err := ChainCommand(t.Context(), given, args, strings.NewReader(""), &output); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !strings.Contains(output.String(), "Celestia") || !strings.Contains(output.String(), filepath.Join(home, "chains.d", "Celestia.yml")) {
			t.Errorf("%v: %s", args, output.String())
		}
	}
	err := ChainCommand(t.Context(), ChainCommandOptions{Home: filepath.Join(home, "missing")}, []string{"list"}, strings.NewReader(""), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("missing home: %v", err)
	}
}

func TestRemovedReloadKeysAreOnlyWarnedAbout(t *testing.T) {
	previous := cliMode
	cliMode = true
	defer func() { cliMode = previous }()
	stderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	c := &Config{}
	err = unmarshalConfig("config.yml", []byte("reload_on_change: yes\nreload_check_seconds: 5\nenable_dashboard: yes\n"), c, &Config{})
	os.Stderr = stderr
	_ = w.Close()
	printed, _ := io.ReadAll(r)
	if err != nil || !c.EnableDash {
		t.Fatalf("the config did not load: %v", err)
	}
	for _, want := range []string{"not understood", "reload_on_change", "reload_check_seconds"} {
		if !strings.Contains(string(printed), want) {
			t.Errorf("warning lacks %q: %s", want, printed)
		}
	}
}

func setSystemPaths(t *testing.T, configFile, directory, stateFile string) {
	t.Helper()
	oldConfig, oldDirectory, oldState := SystemConfigFile, SystemChainDirectory, SystemStateFile
	t.Cleanup(func() {
		SystemConfigFile, SystemChainDirectory, SystemStateFile = oldConfig, oldDirectory, oldState
	})
	SystemConfigFile, SystemChainDirectory, SystemStateFile = configFile, directory, stateFile
}

func TestResolvePathsSystemLayout(t *testing.T) {
	system := t.TempDir()
	config := filepath.Join(system, "config.yml")
	directory := filepath.Join(system, "chains.d")
	state := filepath.Join(t.TempDir(), ".tenderduty-state.json")
	setSystemPaths(t, config, directory, state)
	for _, tc := range []struct {
		name                     string
		cwdConfig, systemConfig  bool
		home, f, cc, state       string
		wantF, wantCC, wantState string
	}{
		{name: "neither config", wantF: "config.yml", wantCC: "chains.d", wantState: ".tenderduty-state.json"},
		{name: "system config", systemConfig: true, wantF: config, wantCC: directory, wantState: state},
		{name: "cwd config wins", cwdConfig: true, systemConfig: true, wantF: "config.yml", wantCC: "chains.d", wantState: ".tenderduty-state.json"},
		{name: "explicit config", systemConfig: true, f: "other.yml", wantF: "other.yml", wantCC: "chains.d", wantState: ".tenderduty-state.json"},
		{name: "explicit home", systemConfig: true, home: system, wantF: config, wantCC: directory, wantState: filepath.Join(system, ".tenderduty-state.json")},
		{name: "explicit chain directory", systemConfig: true, cc: "other.d", wantF: config, wantCC: "other.d", wantState: state},
		{name: "explicit state", systemConfig: true, state: "other.json", wantF: config, wantCC: directory, wantState: "other.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			if tc.cwdConfig {
				if err := os.WriteFile("config.yml", nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.systemConfig {
				if err := os.WriteFile(config, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			wantF, wantCC, wantState := tc.wantF, tc.wantCC, tc.wantState
			if runtime.GOOS == "windows" && !tc.cwdConfig && tc.systemConfig && tc.home == "" && tc.f == "" {
				wantF, wantCC, wantState = "config.yml", "chains.d", ".tenderduty-state.json"
				if tc.cc != "" {
					wantCC = tc.cc
				}
				if tc.state != "" {
					wantState = tc.state
				}
			}
			f, cc, state := ResolvePaths(tc.home, tc.f, tc.cc, tc.state)
			if f != wantF || cc != wantCC || state != wantState {
				t.Fatalf("got %q %q %q, want %q %q %q", f, cc, state, wantF, wantCC, wantState)
			}
		})
	}
}

func TestChainCommandSystemLayout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("system layout is Unix only")
	}
	t.Chdir(t.TempDir())
	options, filename, _ := chainCommandFixture(t)
	setSystemPaths(t, options.ConfigFile, options.Directory, options.StateFile)
	var output strings.Builder
	if err := ChainCommand(t.Context(), ChainCommandOptions{}, []string{"list"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), filename) {
		t.Fatalf("system chain not listed: %s", output.String())
	}
	var reloaded string
	given := ChainCommandOptions{reload: func(stateFile string) (int, error) {
		reloaded = stateFile
		return 1, nil
	}}
	if err := ChainCommand(t.Context(), given, []string{"set", "Celestia", "extra_info=Changed"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	if reloaded != options.StateFile {
		t.Fatalf("reload state %q, want %q", reloaded, options.StateFile)
	}
}
