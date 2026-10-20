package tenderduty

import (
	"regexp"
	"runtime/debug"
	"strings"
)

var pseudoVersion = regexp.MustCompile(`[-.]\d{14}-[0-9a-f]+(\+incompatible)?$`)

// Version returns the release tag or development revision.
func Version() string {
	info, _ := debug.ReadBuildInfo()
	return buildVersion(info)
}

func buildVersion(info *debug.BuildInfo) string {
	if info == nil {
		return "dev"
	}
	v := info.Main.Version
	if strings.HasPrefix(v, "v") && !strings.Contains(v, "-0.") && !strings.Contains(v, "+dirty") && !pseudoVersion.MatchString(v) {
		return v
	}
	var revision string
	var modified bool
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	if len(revision) > 7 {
		revision = revision[:7]
	}
	if modified {
		if revision != "" {
			revision += ", "
		}
		revision += "modified"
	}
	if revision == "" {
		return "dev"
	}
	return "dev (" + revision + ")"
}
