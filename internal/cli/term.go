package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"unsafe"

	"github.com/nalyk/repochat/internal/repo"
)

// console is how commands talk to the person at the terminal. Human-facing
// text goes to errw (stderr) so stdout stays clean for data and protocol.
type console struct {
	in    *os.File
	out   io.Writer // data output (stdout)
	errw  io.Writer // human-facing messages (stderr)
	tty   bool      // stdin and stderr are both terminals: prompting is possible
	color bool
	yes   bool // answer yes to confirmations
	rd    *bufio.Reader
	// interrupted is set when a question was ended by Ctrl+C (or a
	// termination signal) instead of an answer.
	interrupted bool
}

func newConsole(in *os.File, out, errw io.Writer) *console {
	c := &console{in: in, out: out, errw: errw}
	if f, ok := errw.(*os.File); ok && in != nil {
		c.tty = isTerminal(in.Fd()) && isTerminal(f.Fd())
		c.color = isTerminal(f.Fd()) && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
	}
	if in != nil {
		c.rd = bufio.NewReader(in)
	}
	return c
}

func getTermios(fd uintptr) (*syscall.Termios, error) {
	var t syscall.Termios
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(ioctlGetTermios), uintptr(unsafe.Pointer(&t))); e != 0 {
		return nil, e
	}
	return &t, nil
}

func setTermios(fd uintptr, t *syscall.Termios) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(ioctlSetTermios), uintptr(unsafe.Pointer(t))); e != 0 {
		return e
	}
	return nil
}

func isTerminal(fd uintptr) bool {
	_, err := getTermios(fd)
	return err == nil
}

func (c *console) style(code, s string) string {
	if !c.color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (c *console) bold(s string) string  { return c.style("1", s) }
func (c *console) dim(s string) string   { return c.style("2", s) }
func (c *console) green(s string) string { return c.style("32", s) }
func (c *console) red(s string) string   { return c.style("31", s) }
func (c *console) amber(s string) string { return c.style("33", s) }
func (c *console) cyan(s string) string  { return c.style("36", s) }

func (c *console) say(format string, args ...any) {
	fmt.Fprintf(c.errw, format+"\n", args...)
}

func (c *console) ok(format string, args ...any) {
	c.say(c.green("✓")+" "+format, args...)
}

// warn and fail print what went wrong, which is mostly text that came from
// somewhere else: a path, an error from the file system or from another
// process, a value read from the configuration. Their arguments are therefore
// escaped; styled text (c.bold, …) must not be passed to them.
func (c *console) warn(format string, args ...any) {
	c.say(c.amber("!")+" "+format, plain(args)...)
}

func (c *console) fail(format string, args ...any) {
	c.say(c.red("✗")+" "+format, plain(args)...)
}

// plain escapes control characters in the textual arguments of a message.
// Line breaks stay: several messages are laid out over more than one line.
func plain(args []any) []any {
	out := make([]any, len(args))
	for i, a := range args {
		switch t := a.(type) {
		case string:
			out[i] = repo.Sanitize(t)
		case error:
			out[i] = repo.Sanitize(t.Error())
		default:
			out[i] = a
		}
	}
	return out
}

// readLine reads one line of visible input.
func (c *console) readLine() (string, error) {
	if c.rd == nil {
		return "", io.EOF
	}
	line, err := c.rd.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// confirm asks a yes/no question. Without a terminal it never prompts and
// returns the --yes setting.
func (c *console) confirm(question string, def bool) bool {
	if c.yes {
		return true
	}
	if !c.tty {
		return false
	}
	hint := "[y/N]"
	if def {
		hint = "[Y/n]"
	}
	// Ctrl+C at the question means "abort". The commands that ask also turn
	// the signal into a cancelled context, which nothing looks at while a
	// read is blocked: without this the process would sit there until Enter
	// and then take the default answer.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sig)
	fmt.Fprintf(c.errw, "%s %s ", question, hint)
	type answer struct {
		line string
		err  error
	}
	got := make(chan answer, 1)
	go func() {
		line, err := c.readLine()
		got <- answer{line, err}
	}()
	var line string
	select {
	case <-sig:
		c.interrupted = true
		fmt.Fprintln(c.errw)
		return false
	case a := <-got:
		if a.err != nil {
			fmt.Fprintln(c.errw)
			return false
		}
		line = a.line
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "":
		return def
	case "y", "yes", "d", "da":
		return true
	}
	return false
}

const maxSecretInput = 4096

var errInterrupted = errors.New("interrupted")

// readSecret reads one line without echoing it. The terminal's exact prior
// state is restored on every path, including Ctrl+C.
func (c *console) readSecret(prompt string) (string, error) {
	if !c.tty {
		return "", errors.New("no terminal available for a hidden prompt")
	}
	fd := c.in.Fd()
	saved, err := getTermios(fd)
	if err != nil {
		return "", err
	}
	hidden := *saved
	hidden.Lflag &^= syscall.ECHO
	hidden.Lflag |= syscall.ICANON | syscall.ISIG
	if err := setTermios(fd, &hidden); err != nil {
		return "", err
	}
	restore := func() { _ = setTermios(fd, saved) }

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	fmt.Fprint(c.errw, prompt)
	go func() {
		// Read from the buffered reader so no pasted bytes are lost between
		// prompts; cap the line so a stray paste cannot grow without bound.
		var b strings.Builder
		for {
			ch, err := c.rd.ReadByte()
			if err != nil {
				done <- result{b.String(), err}
				return
			}
			if ch == '\n' {
				done <- result{b.String(), nil}
				return
			}
			if b.Len() < maxSecretInput {
				b.WriteByte(ch)
			}
		}
	}()
	select {
	case <-sig:
		signal.Stop(sig)
		restore()
		fmt.Fprintln(c.errw)
		return "", errInterrupted
	case r := <-done:
		signal.Stop(sig)
		restore()
		fmt.Fprintln(c.errw)
		if r.err != nil && r.line == "" {
			return "", r.err
		}
		return strings.TrimRight(r.line, "\r"), nil
	}
}
