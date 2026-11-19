package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
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
	line = append(line, '\n')
	// A process that waited for the lock may be holding a file that another
	// one has just rotated away. Appending to it would be harmless, but
	// judging "too big, rotate" by it would rename the fresh file over the
	// history that was just set aside. So the decision is only ever taken on
	// the file that is at the path now; otherwise open again.
	for attempt := 0; attempt < 5; attempt++ {
		done, err := a.appendOnce(line)
		if done || err != nil {
			return err
		}
	}
	return errors.New("the activity log keeps being rotated by another process")
}

// appendOnce appends line to the log, rotating it first when it is full.
// done is false when the file was rotated by someone else while waiting.
func (a *auditLog) appendOnce(line []byte) (done bool, err error) {
	f, err := os.OpenFile(a.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return false, err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	held, err := f.Stat()
	if err != nil {
		return false, err
	}
	if current, err := os.Lstat(a.path); err != nil || !os.SameFile(held, current) {
		return false, nil
	}
	if held.Size() > auditMaxBytes {
		// Whoever opens the path from now on gets the new file and its own
		// lock; this handle keeps pointing at the rotated one.
		if err := os.Rename(a.path, a.path+".1"); err != nil {
			return false, err
		}
		nf, err := os.OpenFile(a.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
		if err != nil {
			return false, err
		}
		defer nf.Close()
		_, err = nf.Write(line)
		return true, err
	}
	_, err = f.Write(line)
	return true, err
}

// cmdLog prints the most recent activity records.
func cmdLog(c *console, args []string) int {
	fs := flag.NewFlagSet("log", flag.ContinueOnError)
	fs.SetOutput(c.errw)
	n := fs.Int("n", 40, "number of most recent entries to show")
	asJSON := fs.Bool("json", false, "print the raw JSON lines")
	if code, ok := parseOptions(c, fs, args); !ok {
		return code
	}
	a, err := openAudit()
	if err != nil {
		c.fail("%v", err)
		return 1
	}
	var lines []string
	unreadable := 0
	for _, p := range []string{a.path + ".1", a.path} {
		f, err := os.Open(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			c.fail("%v", err)
			return 1
		}
		skipped, err := eachLine(f, maxLogLine, func(line []byte) { lines = append(lines, string(line)) })
		f.Close()
		unreadable += skipped
		if err != nil {
			c.fail("%s: %v", p, err)
			return 1
		}
	}
	if unreadable > 0 {
		c.warn("%d entry(ies) larger than %d KB were skipped", unreadable, maxLogLine>>10)
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

// maxLogLine bounds one activity record when reading the log back.
const maxLogLine = 1 << 20

// eachLine calls fn for every line of r (without its line ending). A line
// longer than max is skipped and counted instead of ending the stream: one
// oversized record must not hide everything that follows it.
func eachLine(r io.Reader, max int, fn func(line []byte)) (skipped int, err error) {
	br := bufio.NewReaderSize(r, 64<<10)
	var line []byte
	tooLong := false
	for {
		chunk, e := br.ReadSlice('\n')
		if !tooLong {
			if len(line)+len(chunk) > max {
				tooLong, line = true, line[:0]
			} else {
				line = append(line, chunk...)
			}
		}
		if errors.Is(e, bufio.ErrBufferFull) {
			continue
		}
		if e != nil && !errors.Is(e, io.EOF) {
			return skipped, e
		}
		switch {
		case tooLong:
			skipped++
		case len(bytes.TrimRight(line, "\r\n")) > 0:
			fn(bytes.TrimRight(line, "\r\n"))
		}
		line, tooLong = line[:0], false
		if e != nil {
			return skipped, nil
		}
	}
}
