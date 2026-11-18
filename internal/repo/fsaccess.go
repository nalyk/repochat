package repo

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
)

var (
	errSymlink    = errors.New("is a symbolic link; symlinks are never followed")
	errNotRegular = errors.New("is not a regular file")
	errChanged    = errors.New("changed while it was being opened; try again")
)

// opener opens working-tree files without ever following a symbolic link.
//
// Every directory on the way is Lstat'ed (it must be a real directory), then
// opened, and the opened handle is fstat'ed and compared with the Lstat
// result; the file itself is Lstat'ed (regular file), opened with
// O_NOFOLLOW|O_NONBLOCK and compared the same way. A component swapped for a
// symlink between the two steps therefore yields an error, never a different
// file. Verified directory handles are cached for the opener's lifetime.
type opener struct {
	r    *Repo
	dirs map[string]*os.Root
}

func (r *Repo) newOpener() *opener { return &opener{r: r, dirs: map[string]*os.Root{}} }

func (o *opener) Close() {
	for _, d := range o.dirs {
		d.Close()
	}
	o.dirs = map[string]*os.Root{}
}

// dir returns a verified handle for the directory rel ("" is the root).
func (o *opener) dir(rel string) (*os.Root, error) {
	if rel == "" {
		return o.r.root, nil
	}
	if d, ok := o.dirs[rel]; ok {
		return d, nil
	}
	parentRel, name := "", rel
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		parentRel, name = rel[:i], rel[i+1:]
	}
	parent, err := o.dir(parentRel)
	if err != nil {
		return nil, err
	}
	li, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if li.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s %w", rel, errSymlink)
	}
	if !li.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", rel)
	}
	d, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	fi, err := d.Stat(".")
	if err != nil || !os.SameFile(li, fi) {
		d.Close()
		return nil, fmt.Errorf("%s %w", rel, errChanged)
	}
	o.dirs[rel] = d
	return d, nil
}

func splitRel(rel string) (dir, name string) {
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		return rel[:i], rel[i+1:]
	}
	return "", rel
}

// Lstat describes rel without following a final symlink.
func (o *opener) Lstat(rel string) (os.FileInfo, error) {
	dirRel, name := splitRel(rel)
	d, err := o.dir(dirRel)
	if err != nil {
		return nil, err
	}
	return d.Lstat(name)
}

// Open opens the regular file rel for reading.
func (o *opener) Open(rel string) (*os.File, os.FileInfo, error) {
	dirRel, name := splitRel(rel)
	d, err := o.dir(dirRel)
	if err != nil {
		return nil, nil, err
	}
	li, err := d.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if li.Mode()&os.ModeSymlink != 0 {
		return nil, nil, fmt.Errorf("%s %w", rel, errSymlink)
	}
	if !li.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s %w", rel, errNotRegular)
	}
	f, err := d.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || !os.SameFile(li, fi) {
		f.Close()
		return nil, nil, fmt.Errorf("%s %w", rel, errChanged)
	}
	return f, fi, nil
}

// ReadAll reads up to limit bytes of the regular file rel. truncated reports
// that the file is larger than limit.
func (o *opener) ReadAll(rel string, limit int64) (data []byte, fi os.FileInfo, truncated bool, err error) {
	f, fi, err := o.Open(rel)
	if err != nil {
		return nil, nil, false, err
	}
	defer f.Close()
	data, err = io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, fi, false, err
	}
	if int64(len(data)) > limit {
		return data[:limit], fi, true, nil
	}
	return data, fi, false, nil
}

// describeErr turns a file-access error into a short message that is safe
// and useful to show to a model (no absolute paths).
func describeErr(rel string, err error) error {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("%s is listed by git but is not present in the working tree (deleted, or excluded by a sparse checkout)", rel)
	case errors.Is(err, errSymlink), errors.Is(err, errNotRegular), errors.Is(err, errChanged):
		return err
	case errors.Is(err, os.ErrPermission):
		return fmt.Errorf("%s cannot be read: permission denied", rel)
	case errors.Is(err, syscall.ELOOP):
		return fmt.Errorf("%s %w", rel, errSymlink)
	default:
		var pe *os.PathError
		if errors.As(err, &pe) {
			return fmt.Errorf("%s cannot be read: %v", rel, pe.Err)
		}
		return fmt.Errorf("%s cannot be read: %v", rel, err)
	}
}
