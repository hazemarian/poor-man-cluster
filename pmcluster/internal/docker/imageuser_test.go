package docker

import "testing"

const testPasswd = `root:x:0:0:root:/root:/bin/sh
app:x:10001:10001:app:/app:/bin/sh
postgres:x:70:70::/var/lib/postgresql:/bin/sh
`

const testGroup = `root:x:0:
app:x:10001:
postgres:x:70:
pgdata:x:1002:
`

func TestParseImageUser(t *testing.T) {
	cases := []struct {
		declared string
		uid, gid int
		found    bool
	}{
		{"10001", 10001, 10001, true},
		{"10001:10001", 10001, 10001, true},
		{"10001:pgdata", 10001, 1002, true},
		{"app", 10001, 10001, true},
		{"app:app", 10001, 10001, true},
		{"app:1002", 10001, 1002, true},
		// a numeric user whose passwd entry names a different primary gid
		{"70", 70, 70, true},
		// names that are not in the image are not guessed at
		{"nobody-here", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, c := range cases {
		uid, gid, found, err := parseImageUser(c.declared, []byte(testPasswd), []byte(testGroup))
		if err != nil {
			t.Fatalf("%q: %v", c.declared, err)
		}
		if found != c.found || (found && (uid != c.uid || gid != c.gid)) {
			t.Errorf("%q => uid=%d gid=%d found=%v, want uid=%d gid=%d found=%v",
				c.declared, uid, gid, found, c.uid, c.gid, c.found)
		}
	}
}

func TestParseImageUserNumericNeedsNoPasswd(t *testing.T) {
	// The common case in this cluster: the image declares numbers, so the repair
	// loop must resolve it without ever creating a container to read /etc/passwd.
	uid, gid, found, err := parseImageUser("10001:10001", nil, nil)
	if err != nil || !found || uid != 10001 || gid != 10001 {
		t.Fatalf("numeric declaration = %d:%d found=%v err=%v", uid, gid, found, err)
	}
}

func TestIsImageRoot(t *testing.T) {
	for _, s := range []string{"", " ", "0", "root", "root:root", "0:0"} {
		if !isImageRoot(s) {
			t.Errorf("%q must count as root", s)
		}
	}
	for _, s := range []string{"app", "10001", "app:app", "65532:65532"} {
		if isImageRoot(s) {
			t.Errorf("%q declares a non-root user", s)
		}
	}
}
