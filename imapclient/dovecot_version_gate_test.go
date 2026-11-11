package imapclient_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Exercise the real version command and skip decision in a subprocess: expected
// Fatal and Skip results must not turn this parent regression into a failure
// or hide it as an integration-test skip.
func TestDovecotNotModSeqVersionGate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a shell stand-in for doveadm")
	}
	for _, tc := range []struct {
		name, output string
		exit         int
		skip, fail   bool
	}{
		{name: "affected", output: "# 2.4.5 (revision): /tmp/dovecot.conf\n", skip: true},
		{name: "earlier", output: "# 2.4.4 (revision): /tmp/dovecot.conf\n"},
		{name: "later", output: "# 2.4.6 (revision): /tmp/dovecot.conf\n"},
		{name: "patch suffix", output: "# 2.4.5.1 (revision): /tmp/dovecot.conf\n"},
		{name: "prefix only", output: "# 2.4.50 (revision): /tmp/dovecot.conf\n"},
		{name: "config failure", exit: 1, fail: true},
		{name: "missing header", output: "2.4.5\n", fail: true},
		{name: "empty version", output: "#\n", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binDir := t.TempDir()
			script := "#!/bin/sh\n" +
				"if [ \"$1\" != '-c' ] || [ \"$3\" != 'config' ] || [ \"$4\" != '-n' ]; then exit 97; fi\n" +
				fmt.Sprintf("printf '%%s' '%s'\nexit %d\n", tc.output, tc.exit)
			if err := os.WriteFile(filepath.Join(binDir, "doveadm"), []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDovecotNotModSeqVersionGateHelper$", "-test.v")
			cmd.Env = append(os.Environ(), "GOIMAP_VERSION_GATE_HELPER=1", "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			out, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("version probe did not finish: %v", ctx.Err())
			}
			if tc.fail {
				if err == nil || !strings.Contains(string(out), "doveadm config:") || strings.Contains(string(out), "--- SKIP:") {
					t.Fatalf("version read should fail without skipping: error=%v, output=%s", err, out)
				}
				return
			}
			if err != nil {
				t.Fatalf("version probe: %v\n%s", err, out)
			}
			skipped := strings.Contains(string(out), "--- SKIP:")
			ran := strings.Contains(string(out), "version-gate-ran")
			if skipped != tc.skip || ran == tc.skip {
				t.Errorf("skip=%v ran=%v, want skip=%v\n%s", skipped, ran, tc.skip, out)
			}
		})
	}
}

func TestDovecotNotModSeqVersionGateHelper(t *testing.T) {
	if os.Getenv("GOIMAP_VERSION_GATE_HELPER") != "1" {
		return
	}
	skipDovecotNotModSeq(t, &dovecotServer{cfgFilename: "/tmp/version-gate-dovecot.conf"})
	fmt.Println("version-gate-ran")
}
