package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// "Is anything being shared right now?" must not depend on what this shell
// happens to know. A share started from environment variables in another
// shell, or from a profile that has been removed since, is still a share.
func TestStatusAndStopSeeSharesThatAreNotSavedProfiles(t *testing.T) {
	e := setup(t, "")
	t.Setenv("CONTROL_PLANE_TUNNEL_ID", testTunnel)
	t.Setenv("CONTROL_PLANE_API_KEY", testKey)
	_, errA, done := start("--root", e.repo)
	waitFor(t, "the share to start", func() bool { return strings.Contains(errA.String(), "connected to OpenAI") })

	// Another shell: no environment credentials, no saved profile.
	t.Setenv("CONTROL_PLANE_TUNNEL_ID", "")
	t.Setenv("CONTROL_PLANE_API_KEY", "")
	out, errText, code := runCLI("status")
	if code != 0 || !strings.Contains(out, "sharing "+e.repo) || strings.Contains(out+errText, "Nothing is being shared") {
		t.Fatalf("status must report the live share: exit %d\nstdout: %s\nstderr: %s", code, out, errText)
	}
	_, errText, code = runCLI("stop")
	if code != 0 || strings.Contains(errText, "Nothing to stop") {
		t.Fatalf("stop must reach the live share: exit %d\n%s", code, errText)
	}
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the share kept running after `repochat stop`")
	}
	if out, _, _ = runCLI("status"); strings.Contains(out, "sharing ") {
		t.Errorf("status after stop: %q", out)
	}
}

func TestStatusRejectsAnUnknownProfile(t *testing.T) {
	setup(t, "")
	saveProfile(t, testKey)
	for _, cmd := range []string{"status", "stop"} {
		_, errText, code := runCLI(cmd, "--profile", "typo")
		if code == 0 || !strings.Contains(errText, `no saved profile named "typo"`) {
			t.Errorf("%s --profile typo: exit %d %q", cmd, code, errText)
		}
	}
}

// If the launcher is killed outright, tunnel-client survives holding the
// tunnel's lock and goes on serving. Nothing answers on the control socket any
// more, but saying "nothing is being shared" would be false.
func TestStatusReportsAShareWhoseLauncherIsGone(t *testing.T) {
	setup(t, "")
	saveProfile(t, testKey)
	dir, err := tunnelRunDir(testTunnel)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if got, err := tryLock(lock); err != nil || !got {
		t.Fatalf("cannot take the lock: %v %v", got, err)
	}
	os.WriteFile(filepath.Join(dir, "owner.json"), []byte(`{"repo":"/work/project","started":"2026-01-01T00:00:00Z","socket":"`+filepath.Join(dir, "ctl.sock")+`"}`), 0o600)

	out, errText, code := runCLI("status")
	if code == 0 || strings.Contains(out+errText, "Nothing is being shared") || !strings.Contains(out+errText, "/work/project") || !strings.Contains(out+errText, "does not answer") {
		t.Fatalf("status must say that the tunnel is still held: exit %d\nstdout: %s\nstderr: %s", code, out, errText)
	}
	_, errText, code = runCLI("stop")
	if code == 0 || strings.Contains(errText, "Nothing to stop") || !strings.Contains(errText, "tunnel-client") {
		t.Fatalf("stop must say what is left running: exit %d\n%s", code, errText)
	}

	if !strings.Contains(errText, "may still be active") {
		t.Errorf("the wording must not claim more than is known: %s", errText)
	}
	// Once the lock is free the leftover files are just leftovers.
	lock.Close()
	out, errText, code = runCLI("status")
	if code != 0 || !strings.Contains(out+errText, "Nothing is being shared") {
		t.Fatalf("stale files must not be reported as a share: exit %d\n%s%s", code, out, errText)
	}
}

// doctor may only tick what it has evidence for. "A tool call was served
// through this tunnel" is evidence about the saved profile's tunnel, not about
// a different tunnel given in the environment.
func TestDoctorDoesNotCreditAnotherTunnelsHistory(t *testing.T) {
	e := setup(t, "")
	cfg := &Config{Profiles: map[string]*Profile{"default": {TunnelID: testTunnel, APIKey: testKey, Onboarded: true}}, DefaultProfile: "default"}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	_, errText, _ := runCLI("doctor", "--root", e.repo, "--offline")
	wantIn(t, errText, "✓ ChatGPT: tool call")

	t.Setenv("CONTROL_PLANE_TUNNEL_ID", "tunnel_ffffffffffffffffffffffffffffffff")
	t.Setenv("CONTROL_PLANE_API_KEY", testKey)
	_, errText, _ = runCLI("doctor", "--root", e.repo, "--offline")
	if strings.Contains(errText, "✓ ChatGPT: tool call") {
		t.Errorf("doctor credited a never-used tunnel with the saved profile's history:\n%s", errText)
	}
}

// While tunnel-client has lost contact with OpenAI the share is not working,
// whatever it had achieved before; status and doctor must not call it healthy.
func TestReportedStateFollowsTheConnection(t *testing.T) {
	l := &launcher{}
	l.setState("serving")
	if got := l.reportedState(); got != "serving" {
		t.Fatalf("state %q", got)
	}
	l.setTrouble(true)
	if got := l.reportedState(); got != stateReconnecting {
		t.Fatalf("with the connection lost the state must be %q, got %q", stateReconnecting, got)
	}
	l.setTrouble(false)
	if got := l.reportedState(); got != "serving" {
		t.Fatalf("after reconnecting: %q", got)
	}
}

// The advice printed when tunnel-client dies must come from what its log
// says, not from digits that happen to occur in a timestamp or a port.
func TestRejectionIsReadFromTheLogNotGuessed(t *testing.T) {
	for log, want := range map[string]int{
		"2026-11-18T10:20:30.340112Z health listening on 127.0.0.1:44013\nwrite state: no space left on device": 0,
		"time=10:04:03.140400 level=error msg=\"mcp command not found: /opt/repochat\"":                         0,
		"exec: \"repochat\": executable file not found in $PATH":                                                0,
		"control plane poll failed: HTTP 401 Unauthorized":                                                      401,
		`{"level":"error","msg":"poll failed","status":403}`:                                                    403,
		"error: unexpected status code 404 from the control plane":                                              404,
		"poll: 403 Forbidden (tunnel permissions)":                                                              403,
		"level=error http_status=401 failure_category=http_error":                                               401,
		"tunnel returned 404 Not Found":                                                                         404,
	} {
		if got := rejectionInLog(log); got != want {
			t.Errorf("rejectionInLog(%q) = %d, want %d", log, got, want)
		}
	}
}

// Looking must not get in the way of starting. While `repochat status` probes
// a tunnel's lock, a launcher starting at that instant must wait for the probe
// rather than conclude that the tunnel is taken.
func TestProbingATunnelDoesNotBlockItsOwner(t *testing.T) {
	setup(t, "")
	dir, err := tunnelRunDir(testTunnel)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "owner.json"), []byte(`{"repo":"/gone","socket":"`+filepath.Join(dir, "ctl.sock")+`"}`), 0o600)
	for i := 0; i < 300; i++ {
		probed := make(chan struct{})
		go func() { held, _ := lockHeld(dir); _ = held; close(probed) }()
		lock, got, err := takeOwnership(dir)
		if err != nil || !got {
			t.Fatalf("round %d: a probe made the tunnel look taken (got %v, err %v)", i, got, err)
		}
		<-probed
		lock.Close()
	}
}
