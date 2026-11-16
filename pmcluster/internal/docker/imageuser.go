package docker

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/container"
)

// ImageUserIDs resolves the numeric uid/gid an image runs as.
//
// The platform needs this to keep app-data directories writable by the app that
// owns them: adoption copies the volume root as root, and a restore writes into
// it as root, so a directory can end up owned by 0 while the image declares a
// non-root USER. Resolving a *name* is the awkward half — "app" means nothing to
// the host — so the image's own /etc/passwd is read from a stopped container it
// creates, which needs no shell and works on distroless images that have a
// passwd file but no /bin/sh.
//
// ok is false when the image runs as root (nothing to repair) or when the user
// cannot be resolved locally (image not cached here, or no such entry). Callers
// treat that as "leave it alone", never as an error to retry.
func (r *realClient) ImageUserIDs(ctx context.Context, ref string) (uid, gid int, ok bool, err error) {
	if r == nil || r.c == nil || ref == "" {
		return 0, 0, false, nil
	}
	img, ierr := r.c.ImageInspect(ctx, ref)
	if ierr != nil {
		// Not on this node, or the tag has been pruned. Not an error worth
		// surfacing every 30 seconds.
		return 0, 0, false, nil
	}
	declared := strings.TrimSpace(img.Config.User)
	if isImageRoot(declared) {
		return 0, 0, false, nil
	}
	// A fully numeric declaration needs no filesystem read at all.
	if u, g, n, perr := parseImageUser(declared, nil, nil); perr == nil && n {
		return u, g, true, nil
	}
	passwd, group, ferr := r.readImageFiles(ctx, ref, "/etc/passwd", "/etc/group")
	if ferr != nil {
		return 0, 0, false, nil
	}
	u, g, n, perr := parseImageUser(declared, passwd, group)
	if perr != nil || !n {
		return 0, 0, false, nil
	}
	return u, g, true, nil
}

// readImageFiles reads files out of an image without starting it: create a
// container from the image, copy the paths out of its filesystem, remove it.
func (r *realClient) readImageFiles(ctx context.Context, ref string, paths ...string) (passwd, group []byte, err error) {
	created, cerr := r.c.ContainerCreate(ctx, &container.Config{Image: ref}, nil, nil, nil, "")
	if cerr != nil {
		return nil, nil, cerr
	}
	defer func() {
		_ = r.c.ContainerRemove(context.WithoutCancel(ctx), created.ID, container.RemoveOptions{Force: true})
	}()
	for _, p := range paths {
		data, rerr := r.readPathFromContainer(ctx, created.ID, p)
		if rerr != nil {
			continue // a missing /etc/group is normal
		}
		switch p {
		case "/etc/passwd":
			passwd = data
		case "/etc/group":
			group = data
		}
	}
	if len(passwd) == 0 {
		return nil, nil, fmt.Errorf("image %s has no readable /etc/passwd", ref)
	}
	return passwd, group, nil
}

// readPathFromContainer copies one file out of a container as raw bytes.
func (r *realClient) readPathFromContainer(ctx context.Context, id, path string) ([]byte, error) {
	rc, _, err := r.c.CopyFromContainer(ctx, id, path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	tr := tar.NewReader(rc)
	for {
		hdr, terr := tr.Next()
		if terr == io.EOF {
			return nil, fmt.Errorf("%s not found in container %s", path, id)
		}
		if terr != nil {
			return nil, terr
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		return io.ReadAll(tr)
	}
}

// isImageRoot reports whether a USER declaration means root (or nothing at all),
// in which case the app can write anywhere and there is nothing to repair.
func isImageRoot(declared string) bool {
	u := strings.SplitN(strings.TrimSpace(declared), ":", 2)[0]
	switch u {
	case "", "0", "root":
		return true
	}
	return false
}

// parseImageUser turns a USER declaration into numbers, using the image's passwd
// and group files for names. A declaration can be "name", "uid", "name:group" or
// "uid:gid"; Docker also accepts "uid:groupname".
//
// found is false (with no error) when a name has no entry — the caller then
// leaves the directory alone rather than guessing an owner.
func parseImageUser(declared string, passwd, group []byte) (uid, gid int, found bool, err error) {
	declared = strings.TrimSpace(declared)
	if declared == "" {
		return 0, 0, false, nil
	}
	userPart, groupPart, hasGroup := strings.Cut(declared, ":")
	userPart = strings.TrimSpace(userPart)

	if n, cerr := strconv.Atoi(userPart); cerr == nil {
		uid = n
		if !hasGroup {
			// gid defaults to the passwd entry's gid when there is one, else the
			// uid: images that only declare a number conventionally use a
			// matching group, and 0 would silently mean root.
			gid = uid
			if g, ok := passwdGID(passwd, n); ok {
				gid = g
			}
			return uid, gid, true, nil
		}
	} else {
		n, ok := passwdUID(passwd, userPart)
		if !ok {
			return 0, 0, false, nil
		}
		uid = n
		if !hasGroup {
			gid = uid
			if g, ok := passwdGID(passwd, uid); ok {
				gid = g
			}
			return uid, gid, true, nil
		}
	}

	groupPart = strings.TrimSpace(groupPart)
	if groupPart == "" {
		return uid, uid, true, nil
	}
	if n, cerr := strconv.Atoi(groupPart); cerr == nil {
		return uid, n, true, nil
	}
	if g, ok := groupGID(group, groupPart); ok {
		return uid, g, true, nil
	}
	return 0, 0, false, nil
}

// passwdUID finds a user's uid in an /etc/passwd body.
func passwdUID(passwd []byte, name string) (int, bool) {
	for _, line := range strings.Split(string(passwd), "\n") {
		f := strings.Split(line, ":")
		if len(f) < 3 || f[0] != name {
			continue
		}
		n, err := strconv.Atoi(f[2])
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// passwdGID finds a uid's primary gid in an /etc/passwd body.
func passwdGID(passwd []byte, uid int) (int, bool) {
	for _, line := range strings.Split(string(passwd), "\n") {
		f := strings.Split(line, ":")
		if len(f) < 4 {
			continue
		}
		n, err := strconv.Atoi(f[2])
		if err != nil || n != uid {
			continue
		}
		g, gerr := strconv.Atoi(f[3])
		if gerr != nil {
			return 0, false
		}
		return g, true
	}
	return 0, false
}

// groupGID finds a group's gid in an /etc/group body.
func groupGID(group []byte, name string) (int, bool) {
	for _, line := range strings.Split(string(group), "\n") {
		f := strings.Split(line, ":")
		if len(f) < 3 || f[0] != name {
			continue
		}
		n, err := strconv.Atoi(f[2])
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}
