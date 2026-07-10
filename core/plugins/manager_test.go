package plugins

import (
	"slices"
	"strings"
	"testing"
)

func hasEnvKey(env []string, key string) bool {
	prefix := key + "="
	return slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, prefix) })
}

func TestMinimalEnv(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("ENOWX_TEST_SECRET", "top-secret")

	env := minimalEnv()

	if !hasEnvKey(env, "PATH") {
		t.Errorf("minimalEnv() = %v, want PATH present", env)
	}
	if hasEnvKey(env, "ENOWX_TEST_SECRET") {
		t.Errorf("minimalEnv() = %v, leaked a non-allow-listed var", env)
	}
}

func TestBuildEnv(t *testing.T) {
	t.Setenv("ENOWX_TEST_SECRET", "top-secret")

	t.Run("default scopes down to the allow-list", func(t *testing.T) {
		man := &Manifest{ID: "demo"}
		env := buildEnv(man, "demo", 5555, 1430)

		if hasEnvKey(env, "ENOWX_TEST_SECRET") {
			t.Errorf("buildEnv() leaked ENOWX_TEST_SECRET into a plugin without env:full")
		}
		if !hasEnvKey(env, "PORT") {
			t.Errorf("buildEnv() = %v, missing PORT", env)
		}
		if !hasEnvKey(env, "ENOWX_PLUGIN_ID") {
			t.Errorf("buildEnv() = %v, missing ENOWX_PLUGIN_ID", env)
		}
		if !hasEnvKey(env, "ENOWX_API") {
			t.Errorf("buildEnv() = %v, missing ENOWX_API", env)
		}
	})

	t.Run("env:full inherits the parent environment", func(t *testing.T) {
		man := &Manifest{ID: "demo", Permissions: []string{"env:full"}}
		env := buildEnv(man, "demo", 5555, 1430)

		if !hasEnvKey(env, "ENOWX_TEST_SECRET") {
			t.Errorf("buildEnv() with env:full did not inherit ENOWX_TEST_SECRET")
		}
	})
}
