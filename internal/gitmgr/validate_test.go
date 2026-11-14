package gitmgr

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeployKeyOpenSSH(t *testing.T) {
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen is not installed")
	}
	k, err := NewDeployKey("wsm-app")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(k.Public, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5") || !strings.HasSuffix(k.Public, " wsm-app") {
		t.Errorf("public key %q", k.Public)
	}
	if !strings.HasPrefix(string(k.Private), "-----BEGIN OPENSSH PRIVATE KEY-----\n") {
		t.Errorf("private key does not have the OpenSSH header")
	}
	path := filepath.Join(t.TempDir(), "id")
	if err := os.WriteFile(path, k.Private, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(keygen, "-y", "-f", path).CombinedOutput()
	if err != nil {
		// The output describes the key file, not the key; it is safe to show.
		t.Fatalf("ssh-keygen -y: %v: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != k.Public {
		t.Errorf("ssh-keygen -y = %q, want %q", got, k.Public)
	}

	other, err := NewDeployKey("wsm-app")
	if err != nil {
		t.Fatal(err)
	}
	if other.Public == k.Public {
		t.Error("two deploy keys are equal")
	}
	if _, err := NewDeployKey("a\nb"); err == nil {
		t.Error("comment with a newline accepted")
	}
}

func TestRepoURLValidation(t *testing.T) {
	tests := []struct {
		url   string
		valid bool
	}{
		{"ssh://git@github.com/owner/repo.git", true},
		{"ssh://git@gitlab.example.com:2222/group/sub/repo.git", true},
		{"ssh://git@[2001:db8::1]:22/repo.git", true},
		{"ssh://deploy_user@10.0.0.5/srv/git/app.git", true},
		{"git@github.com:owner/repo.git", true},
		{"wsm-test-origin@127.0.0.1:app.git", true},
		{"git@host:/srv/repo.git", true},
		{"git@host:~user/repo.git", true},

		{"", false},
		{"-oProxyCommand=evil@host:repo.git", false},
		{"git@-oProxyCommand=evil:repo.git", false},
		{"ssh://-oProxyCommand=evil/repo.git", false},
		{"ssh://git@-oProxyCommand=evil/repo.git", false},
		{"git@host:-upload-pack=evil", false},
		{"ext::sh -c touch% /tmp/pwned", false},
		{"ext::ssh git@host repo", false},
		{"git@host::repo", false},
		{"file:///srv/repo.git", false},
		{"file://host/srv/repo.git", false},
		{"/srv/repo.git", false},
		{"./repo", false},
		{"../repo", false},
		{"repo.git", false},
		{"http://github.com/owner/repo.git", false},
		{"https://github.com/owner/repo.git", false},
		{"https://user@github.com/owner/repo.git", false},
		{"git://github.com/owner/repo.git", false},
		{"ssh://github.com/owner/repo.git", false},
		{"github.com:owner/repo.git", false},
		{"ssh://git@github.com", false},
		{"ssh://git@github.com/", false},
		{"ssh://git@github.com:0/repo.git", false},
		{"ssh://git@github.com:65536/repo.git", false},
		{"ssh://git@github.com:22x/repo.git", false},
		{"ssh://git@github.com:/repo.git", false},
		{"git@host:repo .git", false},
		{"git@host :repo.git", false},
		{"git@host:repo.git\n", false},
		{"git@host:repo\x00.git", false},
		{"git@host:repo\x1b[2J", false},
		{"git@host:repo;rm -rf", false},
		{"git@host:$(id)", false},
		{"git@host:`id`", false},
		{"git@host:../../etc/passwd", false},
		{"git@host:", false},
		{"@host:repo.git", false},
		{"g it@host:repo.git", false},
		{"ssh://git@host/" + strings.Repeat("a", MaxRepoURL), false},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			err := ValidRepoURL(tt.url)
			if (err == nil) != tt.valid {
				t.Errorf("ValidRepoURL(%q) = %v, want valid %v", tt.url, err, tt.valid)
			}
		})
	}
}

func TestBranchValidation(t *testing.T) {
	tests := []struct {
		branch string
		valid  bool
	}{
		{"main", true},
		{"master", true},
		{"develop", true},
		{"release/1.2", true},
		{"feature/x_y-z", true},
		{"v1.0", true},

		{"", false},
		{"-main", false},
		{"--upload-pack=evil", false},
		{"HEAD", false},
		{"main..x", false},
		{"main.", false},
		{".hidden", false},
		{"a/.b", false},
		{"a//b", false},
		{"/main", false},
		{"main/", false},
		{"main.lock", false},
		{"a.lock/b", false},
		{"main branch", false},
		{"main\n", false},
		{"ma~in", false},
		{"ma^in", false},
		{"ma:in", false},
		{"ma?in", false},
		{"ma*in", false},
		{"ma[in", false},
		{"ma\\in", false},
		{"main@{1}", false},
		{"@", false},
		{strings.Repeat("a", MaxBranch+1), false},
	}
	for _, tt := range tests {
		t.Run(tt.branch, func(t *testing.T) {
			err := ValidBranch(tt.branch)
			if (err == nil) != tt.valid {
				t.Errorf("ValidBranch(%q) = %v, want valid %v", tt.branch, err, tt.valid)
			}
		})
	}
}
