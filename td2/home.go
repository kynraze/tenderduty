package tenderduty

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// HomeEnv names the environment variable that sets the home directory when -home is absent.
const HomeEnv = "TENDERDUTY_HOME"

// System paths match the systemd service layout.
var (
	SystemConfigFile     = "/etc/tenderduty/config.yml"
	SystemChainDirectory = "/etc/tenderduty/chains.d"
	SystemStateFile      = "/var/lib/tenderduty/.tenderduty-state.json"
)

// ResolveHome expands a leading ~/ and checks that the directory exists. "" means no home.
func ResolveHome(home string) (string, error) {
	if home == "" {
		return "", nil
	}
	if home == "~" || strings.HasPrefix(home, "~/") || strings.HasPrefix(home, `~\`) {
		if user, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(user, home[1:])
		}
	}
	info, err := os.Stat(home)
	switch {
	case os.IsNotExist(err):
		return "", fmt.Errorf("home directory %s does not exist", home)
	case err != nil:
		return "", err
	case !info.IsDir():
		return "", fmt.Errorf("home directory %s is not a directory", home)
	}
	return home, nil
}

// ResolvePaths fills missing paths from home, the system layout or the working directory.
// A given path is used as it is, so a relative one stays relative to the working directory.
func ResolvePaths(home, configFile, directory, stateFile string) (string, string, string) {
	if home != "" && filepath.Clean(home) == filepath.Dir(SystemConfigFile) && stateFile == "" {
		stateFile = SystemStateFile
	}
	if runtime.GOOS != "windows" && home == "" && configFile == "" {
		if _, err := os.Stat("config.yml"); os.IsNotExist(err) {
			if _, err := os.Stat(SystemConfigFile); err == nil {
				if directory == "" {
					directory = SystemChainDirectory
				}
				if stateFile == "" {
					stateFile = SystemStateFile
				}
				return SystemConfigFile, directory, stateFile
			}
		}
	}
	pick := func(given, name string) string {
		switch {
		case given != "":
			return given
		case home != "":
			return filepath.Join(home, name)
		}
		return name
	}
	return pick(configFile, "config.yml"), pick(directory, "chains.d"), pick(stateFile, ".tenderduty-state.json")
}
