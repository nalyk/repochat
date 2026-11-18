package cli

import (
	"strings"
	"testing"
	"time"
)

const secondTunnel = "tunnel_ffffffffffffffffffffffffffffffff"

// Two repositories can be shared at the same time, each through its own
// tunnel (profile). The instances must not disturb each other, and what each
// saves to the shared configuration must not undo the other's changes.
func TestTwoRepositoriesInParallelThroughTwoTunnels(t *testing.T) {
	e := setup(t, "")
	second := makeRepo(t, "other")
	cfg := &Config{DefaultProfile: "default", Profiles: map[string]*Profile{
		"default": {TunnelID: testTunnel, APIKey: testKey, TunnelName: "First"},
	}}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}

	_, errA, doneA := start("--root", e.repo)
	waitFor(t, "first instance connected", func() bool { return strings.Contains(errA.String(), "connected to OpenAI") })

	// The second tunnel is set up while the first instance is running.
	t.Setenv("CONTROL_PLANE_TUNNEL_ID", secondTunnel)
	t.Setenv("CONTROL_PLANE_API_KEY", testKey)
	if _, errText, code := runCLI("setup", "--profile", "two"); code != 0 {
		t.Fatalf("setup of the second profile failed: %s", errText)
	}
	t.Setenv("CONTROL_PLANE_TUNNEL_ID", "")
	t.Setenv("CONTROL_PLANE_API_KEY", "")

	_, errB, doneB := start("--root", second, "--profile", "two")
	waitFor(t, "second instance served a call", func() bool { return strings.Contains(errB.String(), "setup is complete") })
	waitFor(t, "first instance served a call", func() bool { return strings.Contains(errA.String(), "setup is complete") })
	if strings.Contains(errB.String(), "already in use") || strings.Contains(errA.String(), "taking over") {
		t.Fatalf("instances on different tunnels must not compete:\n%s\n---\n%s", errA.String(), errB.String())
	}
	wantIn(t, errA.String(), "repository  project")
	wantIn(t, errB.String(), "repository  other", "profile two")

	out, _, _ := runCLI("status")
	if !strings.Contains(out, "sharing "+e.repo) || !strings.Contains(out, "sharing "+second) {
		t.Errorf("status must list both:\n%s", out)
	}

	// Stopping one leaves the other running.
	runCLI("stop", "--profile", "two")
	select {
	case <-doneB:
	case <-time.After(20 * time.Second):
		t.Fatal("the second instance did not stop")
	}
	select {
	case <-doneA:
		t.Fatal("stopping one profile must not stop the other")
	case <-time.After(500 * time.Millisecond):
	}
	out, _, _ = runCLI("status")
	if !strings.Contains(out, "sharing "+e.repo) || strings.Contains(out, "sharing "+second) {
		t.Errorf("status after stopping one:\n%s", out)
	}
	runCLI("stop")
	<-doneA

	// Neither instance may have overwritten what the other (or setup) saved.
	final, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	for name, repo := range map[string]string{"default": e.repo, "two": second} {
		p := final.Profiles[name]
		if p == nil {
			t.Fatalf("profile %q was lost from the configuration: %+v", name, final.profileNames())
		}
		if !p.Onboarded || p.LastRepo != repo || p.APIKey != testKey {
			t.Errorf("profile %q lost its updates: onboarded=%v last=%q", name, p.Onboarded, p.LastRepo)
		}
	}
	if final.Profiles["two"].TunnelID != secondTunnel || final.Profiles["default"].TunnelID != testTunnel {
		t.Error("tunnel ids were mixed up")
	}
}

// A running instance holds a copy of the configuration read at start. When it
// later records something, it must not write that old copy back over changes
// made since by another instance or by `repochat setup`.
func TestConfigUpdatesDoNotUndoEachOther(t *testing.T) {
	setup(t, "")
	saveProfile(t, testKey)
	stale, err := loadConfig() // what a long-running instance has in memory
	if err != nil || len(stale.Profiles) != 1 {
		t.Fatal(err)
	}
	// Meanwhile another process adds a profile.
	if _, err := updateConfig(func(c *Config) { c.Profiles["two"] = &Profile{TunnelID: secondTunnel, APIKey: testKey} }); err != nil {
		t.Fatal(err)
	}
	// The long-running instance records its first served call and repository.
	if err := updateProfile("default", testTunnel, func(p *Profile) { p.Onboarded, p.LastRepo = true, "/repo/a" }); err != nil {
		t.Fatal(err)
	}
	final, _ := loadConfig()
	if final.Profiles["two"] == nil {
		t.Fatal("a profile added by another process was wiped out")
	}
	if p := final.Profiles["default"]; !p.Onboarded || p.LastRepo != "/repo/a" {
		t.Errorf("the update itself was lost: %+v", p)
	}
	// An update for a profile that was replaced by a different tunnel is dropped.
	updateConfig(func(c *Config) { c.Profiles["default"].TunnelID = secondTunnel; c.Profiles["default"].LastRepo = "" })
	updateProfile("default", testTunnel, func(p *Profile) { p.LastRepo = "/stale" })
	if final, _ = loadConfig(); final.Profiles["default"].LastRepo != "" {
		t.Error("an update meant for another tunnel was applied")
	}

	// Many concurrent writers: every change must survive.
	done := make(chan error, 20)
	for i := 0; i < 20; i++ {
		name := "p" + string(rune('a'+i))
		go func() {
			_, err := updateConfig(func(c *Config) { c.Profiles[name] = &Profile{TunnelID: secondTunnel, APIKey: testKey} })
			done <- err
		}()
	}
	for i := 0; i < 20; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if final, _ = loadConfig(); len(final.Profiles) != 22 {
		t.Errorf("concurrent updates were lost: %d profiles, want 22", len(final.Profiles))
	}
}
