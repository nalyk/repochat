package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

const (
	auditFile     = "activity.jsonl"
	auditMaxBytes = 5 << 20 // rotated to activity.jsonl.1; at most two files are kept
)

// auditLog appends one JSON line per event to a private local file. It is a
// diagnostic history for the user, not a tamper-proof audit trail. By default
// it records metadata only (tool, outcome, sizes, timings).
type auditLog struct {
	path string
}

func openAudit() (*auditLog, error) {
	dir, err := stateDir()
	if err != nil {
		return nil, err
	}
	if err := privateDir(dir); err != nil {
		return nil, err
	}
	return &auditLog{path: filepath.Join(dir, auditFile)}, nil
}

// write appends a record. Several repochat processes may share the file, so
// rotation and the append happen under an exclusive lock.
func (a *auditLog) write(rec activity) error {
	rec.Summary = ""
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(a.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	if fi, err := f.Stat(); err == nil && fi.Size() > auditMaxBytes {
		// Rotate, then start a fresh file. The handle we hold now points at
		// the rotated file; reopen to write to the new one.
		if err := os.Rename(a.path, a.path+".1"); err == nil {
			nf, err := os.OpenFile(a.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
			if err != nil {
				return err
			}
			defer nf.Close()
			_, err = nf.Write(append(line, '\n'))
			return err
		}
	}
	_, err = f.Write(append(line, '\n'))
	return err
}

// cmdLog prints the most recent activity records.
func cmdLog(c *console, args []string) int {
	fs := flag.NewFlagSet("log", flag.ContinueOnError)
	fs.SetOutput(c.errw)
	n := fs.Int("n", 40, "number of most recent entries to show")
	asJSON := fs.Bool("json", false, "print the raw JSON lines")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	a, err := openAudit()
	if err != nil {
		c.fail("%v", err)
		return 1
	}
	var lines []string
	for _, p := range []string{a.path + ".1", a.path} {
		f, err := os.Open(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			c.fail("%v", err)
			return 1
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			lines = append(lines, sc.Text())
		}
		f.Close()
	}
	if len(lines) == 0 {
		c.say("No activity recorded yet (%s).", a.path)
		return 0
	}
	if *n > 0 && len(lines) > *n {
		lines = lines[len(lines)-*n:]
	}
	for _, l := range lines {
		if *asJSON {
			fmt.Fprintln(c.out, l)
			continue
		}
		var rec activity
		if json.Unmarshal([]byte(l), &rec) != nil {
			continue
		}
		if rec.Args != nil {
			rec.Summary = summarizeArgs(rec.Args, nil)
		}
		date := rec.Time.Local().Format("2006-01-02")
		fmt.Fprintf(c.out, "%s %s  %s\n", date, formatActivity(c, rec), c.dim(rec.Repo))
	}
	c.say(c.dim("log file: %s"), a.path)
	return 0
}
