package tenderduty

import (
	"runtime/debug"
	"testing"
)

func TestBuildVersion(t *testing.T) {
	for _, tc := range []struct {
		name, version, revision, modified, want string
	}{
		{"release", "v2.3.0", "5a704f5123456", "false", "v2.3.0"},
		{"prerelease", "v2.3.0-rc.1", "", "", "v2.3.0-rc.1"},
		{"development", "(devel)", "5a704f5123456", "false", "dev (5a704f5)"},
		{"modified", "(devel)", "5a704f5123456", "true", "dev (5a704f5, modified)"},
		{"no revision", "", "", "", "dev"},
		{"modified only", "(devel)", "", "true", "dev (modified)"},
		{"short revision", "(devel)", "abc", "false", "dev (abc)"},
		{"dirty tag", "v2.3.0+dirty", "5a704f5123456", "true", "dev (5a704f5, modified)"},
		{"pseudo", "v2.3.1-0.20261020120000-5a704f512345", "5a704f5123456", "false", "dev (5a704f5)"},
		{"pseudo without tag", "v2.0.0-20261020120000-5a704f512345", "", "", "dev"},
		{"pseudo prerelease", "v2.3.0-rc.1.0.20261020120000-5a704f512345", "", "", "dev"},
		{"tarball", "(devel)", "", "", "dev"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := &debug.BuildInfo{
				Main: debug.Module{Version: tc.version},
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: tc.revision},
					{Key: "vcs.modified", Value: tc.modified},
				},
			}
			if got := buildVersion(info); got != tc.want {
				t.Fatalf("buildVersion() = %q, want %q", got, tc.want)
			}
		})
	}
	if got := buildVersion(nil); got != "dev" {
		t.Fatalf("buildVersion(nil) = %q", got)
	}
}
