package repo

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
)

// Writing goes through the same rule as reading: no symbolic link is ever
// followed, at any element of the path. Unlike reads, a write does not trust
// directory handles cached by earlier calls: every call walks the path again
// from the root and compares each directory it opens with what the name
// denotes at that moment, so a directory swapped for a link between two
// writes is noticed rather than written through.

// walk opens the directory rel ("" is the root) one element at a time, each
// checked to be a real directory, and returns its handle with a function that
// releases what walk opened. With create, missing directories are made.
func (o *opener) walk(rel string, create bool) (*os.Root, func(), error) {
	var opened []*os.Root
	done := func() {
		for _, d := range opened {
			d.Close()
		}
	}
	if err := o.r.checkRoot(); err != nil {
		return nil, done, err
	}
	cur := o.r.root
	if rel == "" {
		return cur, done, nil
	}
	sofar := ""
	for _, name := range strings.Split(rel, "/") {
		if sofar == "" {
			sofar = name
		} else {
			sofar += "/" + name
		}
		li, err := cur.Lstat(name)
		if create && errors.Is(err, os.ErrNotExist) {
			if err = cur.Mkdir(name, 0o755); err == nil || errors.Is(err, os.ErrExist) {
				li, err = cur.Lstat(name)
			}
		}
		if err != nil {
			return nil, done, err
		}
		if li.Mode()&os.ModeSymlink != 0 {
			return nil, done, fmt.Errorf("%s %w", sofar, errSymlink)
		}
		if !li.IsDir() {
			return nil, done, fmt.Errorf("%s is not a directory", sofar)
		}
		d, err := cur.OpenRoot(name)
		if err != nil {
			return nil, done, err
		}
		opened = append(opened, d)
		fi, err := d.Stat(".")
		if err != nil || !os.SameFile(li, fi) {
			return nil, done, fmt.Errorf("%s %w", sofar, errChanged)
		}
		cur = d
	}
	return cur, done, nil
}

// writable checks a path given to a write: repository-relative, well formed,
// and outside .git.
func writable(rel string) error {
	if !exactPath(rel) {
		return fmt.Errorf("%q is not a path that can be written", SanitizeLine(rel))
	}
	return nil
}

// WriteAtomic replaces (or creates) the regular file rel with data and the
// given permission bits. The content goes to a temporary file in the same
// directory, which is then renamed over the target, so the file is never
// seen half written. Missing parent directories are created. A symbolic link
// at any element of the path, or a target that is not a regular file, is an
// error; on any error no temporary file is left behind.
func (o *opener) WriteAtomic(rel string, data []byte, mode os.FileMode) (err error) {
	if err := writable(rel); err != nil {
		return err
	}
	defer o.Close() // cached read handles may name directories this call made
	dirRel, name := splitRel(rel)
	d, done, err := o.walk(dirRel, true)
	defer done()
	if err != nil {
		return err
	}
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	tmp := ".repochat-" + hex.EncodeToString(rnd[:]) + ".tmp"
	f, err := d.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	renamed := false
	defer func() {
		if !renamed {
			d.Remove(tmp)
		}
	}()
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(mode.Perm())
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	// The target is looked at last, right before it is replaced.
	switch li, err := d.Lstat(name); {
	case err == nil && li.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("%s %w", rel, errSymlink)
	case err == nil && !li.Mode().IsRegular():
		return fmt.Errorf("%s %w", rel, errNotRegular)
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return err
	}
	if err := d.Rename(tmp, name); err != nil {
		return err
	}
	renamed = true
	return nil
}

// Remove deletes the regular file rel, then each parent directory that is
// left empty, up to but not including the root. A symbolic link at any
// element of the path is an error, as is a target that is not a regular file.
func (o *opener) Remove(rel string) error {
	if err := writable(rel); err != nil {
		return err
	}
	defer o.Close() // cached read handles may name directories this call removed
	dirRel, name := splitRel(rel)
	d, done, err := o.walk(dirRel, false)
	if err != nil {
		done()
		return err
	}
	li, err := d.Lstat(name)
	switch {
	case err != nil:
	case li.Mode()&os.ModeSymlink != 0:
		err = fmt.Errorf("%s %w", rel, errSymlink)
	case !li.Mode().IsRegular():
		err = fmt.Errorf("%s %w", rel, errNotRegular)
	default:
		err = d.Remove(name)
	}
	done()
	if err != nil {
		return err
	}
	for dirRel != "" {
		parentRel, base := splitRel(dirRel)
		p, done, err := o.walk(parentRel, false)
		if err == nil {
			var li os.FileInfo
			if li, err = p.Lstat(base); err == nil && li.IsDir() && li.Mode()&os.ModeSymlink == 0 {
				err = p.Remove(base) // fails when the directory is not empty
			} else if err == nil {
				err = errNotRegular
			}
		}
		done()
		if err != nil {
			break
		}
		dirRel = parentRel
	}
	return nil
}
