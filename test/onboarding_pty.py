#!/usr/bin/env python3
"""First-run onboarding, driven through a real pseudo-terminal.

Usage: onboarding_pty.py <repochat binary> <internal/cli test binary>

Starts `repochat` (from a SUBDIRECTORY of a fresh repository, with no saved
configuration) the way a new user would, answers the single hidden prompt and
checks that the flow reaches "setup is complete", that the pasted secret is
never echoed, that the configuration is saved privately and that Ctrl+C stops
cleanly. tunnel-client is replaced by the stand-in built into the test binary,
so no OpenAI account or network is needed.
"""
import json, os, pty, re, select, shutil, stat, subprocess, sys, tempfile, time

TUNNEL = "tunnel_0123456789abcdef0123456789abcdef"
KEY = "sk-test-0123456789abcdefghijklmnop"


def main():
    repochat, testbin = (os.path.abspath(p) for p in sys.argv[1:3])
    work = tempfile.mkdtemp(prefix="repochat-onboarding-")
    try:
        return run(repochat, testbin, work)
    finally:
        shutil.rmtree(work, ignore_errors=True)


def run(repochat, testbin, work):
    home, proj = os.path.join(work, "home"), os.path.join(work, "proj")
    os.makedirs(os.path.join(proj, "src"))
    os.makedirs(home)
    files = {"README.md": "# Demo\n", ".env": "SECRET=NOPE\n", "src/main.go": "package main\nfunc main() {}\n"}
    for name, content in files.items():
        with open(os.path.join(proj, name), "w") as f:
            f.write(content)
    git = ["git", "-c", "user.name=A", "-c", "user.email=a@example.com"]
    subprocess.run(git + ["init", "-q"], cwd=proj, check=True)
    subprocess.run(git + ["symbolic-ref", "HEAD", "refs/heads/main"], cwd=proj, check=True)  # init -b needs git 2.28
    subprocess.run(git + ["add", "-A"], cwd=proj, check=True)
    subprocess.run(git + ["commit", "-q", "-m", "first"], cwd=proj, check=True)

    helper = os.path.join(work, "tunnel-client")
    with open(helper, "w") as f:
        f.write("#!/bin/sh\nREPOCHAT_TEST_ROLE=tunnel-client FAKE_RECORD='%s' FAKE_GOOD_KEY='%s' FAKE_MODE='' exec '%s' \"$@\"\n"
                % (os.path.join(work, "record.json"), KEY, testbin))
    os.chmod(helper, 0o755)

    env = {"PATH": os.environ["PATH"], "HOME": home, "TERM": "xterm", "NO_COLOR": "1",
           "REPOCHAT_CONFIG_DIR": os.path.join(home, "cfg"), "REPOCHAT_STATE_DIR": os.path.join(home, "st"),
           "REPOCHAT_DATA_DIR": os.path.join(home, "data"), "REPOCHAT_TUNNEL_CLIENT": helper}
    pid, fd = pty.fork()
    if pid == 0:
        os.chdir(os.path.join(proj, "src"))
        os.execve(repochat, [repochat], env)

    buf = b""

    def until(pattern, timeout=40):
        nonlocal buf
        end = time.time() + timeout
        while time.time() < end:
            if re.search(pattern, buf.decode("utf-8", "replace")):
                return True
            ready, _, _ = select.select([fd], [], [], 0.2)
            if ready:
                try:
                    data = os.read(fd, 65536)
                except OSError:
                    return False
                if not data:
                    return False
                buf += data
        return False

    checks = {}
    checks["asks once for both values"] = until(r"Paste the tunnel ID and the API key")
    # Nothing may be asked visibly before the hidden prompt: a user who pastes
    # early must never see the secret echoed.
    checks["asks nothing before the hidden prompt"] = "[Y/n]" not in buf.decode("utf-8", "replace") and "[y/N]" not in buf.decode("utf-8", "replace")
    os.write(fd, (KEY + "  " + TUNNEL + "\n").encode())  # any order
    checks["verifies and saves"] = until(r"saved to .*config\.json")
    checks["shows the one-time ChatGPT steps"] = until(r"add the plugin in ChatGPT")
    checks["confirms the first served tool call"] = until(r"setup is complete")
    os.write(fd, b"\x03")
    checks["stops cleanly on Ctrl+C"] = until(r"Stopped\.")
    _, status = os.waitpid(pid, 0)
    text = buf.decode("utf-8", "replace")
    checks["exits with status 0"] = os.waitstatus_to_exitcode(status) == 0
    checks["never echoes the API key"] = KEY not in text
    checks["never echoes the tunnel ID"] = TUNNEL not in text
    checks["found the repository root from a subdirectory"] = re.search(r"repository\s+proj", text) is not None

    cfg_path = os.path.join(home, "cfg", "config.json")
    try:
        cfg = json.load(open(cfg_path))
        profile = cfg["profiles"]["default"]
        checks["saved the profile"] = profile["tunnel_id"] == TUNNEL and profile["api_key"] == KEY
        checks["marked onboarding complete"] = profile.get("onboarded") is True
        checks["config file is 0600"] = stat.S_IMODE(os.stat(cfg_path).st_mode) == 0o600
        checks["config directory is 0700"] = stat.S_IMODE(os.stat(os.path.dirname(cfg_path)).st_mode) == 0o700
    except Exception as exc:  # noqa: BLE001
        checks["saved the profile"] = False
        print("config:", exc)

    failed = [name for name, ok in checks.items() if not ok]
    for name, ok in checks.items():
        print(("ok   " if ok else "FAIL ") + name)
    if failed:
        print("\n----- terminal output -----\n" + text.replace("\r\n", "\n"))
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
