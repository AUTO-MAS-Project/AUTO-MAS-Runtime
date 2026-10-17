package macosprobe

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const probeBudget = 20 * time.Second

type probeHello struct {
	Role      string
	Token     string
	PID       int
	ParentPID int
	Group     int
}

type probePeer struct {
	pid  int
	conn *net.UnixConn
}

type launchdProbe struct {
	guard      probePeer
	worker     probePeer
	grandchild probePeer
	escaped    probePeer
	queue      int
	exited     map[int]bool
}

func startLaunchdProbe(t *testing.T) *launchdProbe {
	t.Helper()
	// Darwin 的 sockaddr_un 有严格长度上限；仅测试使用短临时根，生产 IPC 另行设计。
	dir, err := os.MkdirTemp("/tmp", "auto-mas-p0-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(dir); err != nil {
			t.Error(err)
		}
	})
	for _, name := range []string{"probe.plist", "guard.log", "owner.sock"} {
		path := filepath.Join(dir, name)
		t.Cleanup(func() {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Error(err)
			}
		})
	}
	t.Cleanup(func() { t.Logf("isolated prototype root: %s", dir) })
	return openLaunchdProbe(t, dir)
}

func openLaunchdProbe(t *testing.T, dir string) *launchdProbe {
	t.Helper()
	socket := filepath.Join(dir, "owner.sock")
	if len(socket) >= 104 {
		t.Fatalf("got socket path length %d, want below 104", len(socket))
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := os.Chmod(socket, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := listener.SetDeadline(time.Now().Add(probeBudget)); err != nil {
		t.Fatal(err)
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	token := hex.EncodeToString(nonce[:])
	label := "org.auto-mas.runtime.probe." + token
	domain := "user/" + strconv.Itoa(os.Getuid())
	target := domain + "/" + label
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	args := []string{executable, "-test.run=^TestLaunchd_Helper$", "--", "guard", socket, token, strconv.Itoa(os.Getpid())}
	plist := probePlist(label, args, filepath.Join(dir, "guard.log"))
	plistPath := filepath.Join(dir, "probe.plist")
	if err := os.WriteFile(plistPath, []byte(plist), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), probeBudget)
	defer cancel()
	bootstrapped := false
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), probeBudget)
		defer cleanupCancel()
		output, err := exec.CommandContext(cleanupCtx, "/bin/launchctl", "bootout", target).CombinedOutput()
		if err != nil && bootstrapped {
			t.Errorf("bootout %s: %v: %s", target, err, output)
		}
		if log, err := os.ReadFile(filepath.Join(dir, "guard.log")); err == nil && len(log) > 0 {
			t.Logf("launchd guard output: %s", log)
		}
		if output, err := exec.CommandContext(cleanupCtx, "/bin/launchctl", "print", target).CombinedOutput(); err == nil {
			t.Errorf("job still registered after bootout: %s", output)
		}
	})
	output, err := exec.CommandContext(ctx, "/bin/launchctl", "bootstrap", domain, plistPath).CombinedOutput()
	if err != nil {
		t.Fatalf("bootstrap %s: %v: %s", target, err, output)
	}
	bootstrapped = true
	queue, err := unix.Kqueue()
	if err != nil {
		t.Fatal(err)
	}
	unix.CloseOnExec(queue)
	t.Cleanup(func() {
		if err := unix.Close(queue); err != nil {
			t.Error(err)
		}
	})
	probe := &launchdProbe{queue: queue, exited: make(map[int]bool)}
	groups := make(map[string]int)
	parents := make(map[string]int)
	for range 4 {
		conn, err := listener.AcceptUnix()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			// 测试动作可能已经关闭连接；重复关闭不再拥有资源。
			if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				t.Error(err)
			}
		})
		if err := conn.SetDeadline(time.Now().Add(probeBudget)); err != nil {
			t.Fatal(err)
		}
		var hello probeHello
		if err := json.NewDecoder(io.LimitReader(conn, 4096)).Decode(&hello); err != nil {
			t.Fatal(err)
		}
		if hello.Token != token || hello.PID <= 0 {
			t.Fatalf("got invalid peer hello %+v", hello)
		}
		if err := verifyPeer(conn, hello.PID); err != nil {
			t.Fatal(err)
		}
		peer := probePeer{pid: hello.PID, conn: conn}
		switch hello.Role {
		case "guard":
			if probe.guard.pid != 0 {
				t.Fatal("duplicate guard")
			}
			probe.guard = peer
		case "worker":
			if probe.worker.pid != 0 {
				t.Fatal("duplicate worker")
			}
			probe.worker = peer
		case "escaped":
			if probe.escaped.pid != 0 {
				t.Fatal("duplicate escaped worker")
			}
			probe.escaped = peer
		case "grandchild":
			if probe.grandchild.pid != 0 {
				t.Fatal("duplicate grandchild")
			}
			probe.grandchild = peer
		default:
			t.Fatalf("got role %q, want guard/worker/grandchild/escaped", hello.Role)
		}
		groups[hello.Role] = hello.Group
		parents[hello.Role] = hello.ParentPID
		change := unix.Kevent_t{Ident: uint64(hello.PID), Filter: unix.EVFILT_PROC, Flags: unix.EV_ADD | unix.EV_ENABLE, Fflags: unix.NOTE_EXIT}
		if _, err := unix.Kevent(queue, []unix.Kevent_t{change}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if probe.guard.pid == 0 || probe.worker.pid == 0 || probe.escaped.pid == 0 || probe.grandchild.pid == 0 {
		t.Fatal("missing peer role")
	}
	if groups["guard"] != groups["worker"] || groups["guard"] != groups["grandchild"] || groups["guard"] == groups["escaped"] || parents["worker"] != probe.guard.pid || parents["escaped"] != probe.guard.pid || parents["grandchild"] != probe.worker.pid {
		t.Fatalf("got groups=%v parents=%v, want inherited worker and explicit escape", groups, parents)
	}
	t.Logf("domain=%s guard=%d worker=%d escaped=%d groups=%v", domain, probe.guard.pid, probe.worker.pid, probe.escaped.pid, groups)
	t.Cleanup(func() {
		if !probe.exited[probe.escaped.pid] {
			// 只关闭由本测试创建并认证的会话，不按名称或快照 PID 批量杀进程。
			if err := probe.escaped.conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				t.Error(err)
			}
			probe.waitExited(t, probe.escaped.pid)
		}
	})
	return probe
}

func (p *launchdProbe) waitExited(t *testing.T, pids ...int) {
	t.Helper()
	deadline := time.Now().Add(probeBudget)
	for {
		complete := true
		for _, pid := range pids {
			complete = complete && p.exited[pid]
		}
		if complete {
			return
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("got exit events %v, want %v", p.exited, pids)
		}
		var events [8]unix.Kevent_t
		timeout := unix.NsecToTimespec(remaining.Nanoseconds())
		n, err := unix.Kevent(p.queue, nil, events[:], &timeout)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events[:n] {
			if event.Flags&unix.EV_ERROR != 0 {
				t.Fatalf("kqueue error %d", event.Data)
			}
			if event.Fflags&unix.NOTE_EXIT != 0 {
				p.exited[int(event.Ident)] = true
			}
		}
	}
}

func (p *launchdProbe) assertEscapedAlive(t *testing.T) {
	t.Helper()
	if p.exited[p.escaped.pid] {
		t.Fatal("explicitly detached worker exited with guard")
	}
	if err := json.NewEncoder(p.escaped.conn).Encode("ping"); err != nil {
		t.Fatal(err)
	}
	var response string
	if err := json.NewDecoder(p.escaped.conn).Decode(&response); err != nil || response != "pong" {
		t.Fatalf("got detached response %q, %v, want pong", response, err)
	}
}

func probePlist(label string, args []string, logPath string) string {
	var out bytes.Buffer
	out.WriteString(`<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict><key>Label</key><string>`)
	// bytes.Buffer 的 Write 不失败；EscapeText 只对这些已验证字符串做 XML 编码。
	_ = xml.EscapeText(&out, []byte(label))
	out.WriteString(`</string><key>ProgramArguments</key><array>`)
	for _, arg := range args {
		out.WriteString("<string>")
		_ = xml.EscapeText(&out, []byte(arg))
		out.WriteString("</string>")
	}
	out.WriteString(`</array><key>RunAtLoad</key><true/><key>KeepAlive</key><false/><key>AbandonProcessGroup</key><false/><key>ExitTimeOut</key><integer>2</integer>`)
	for _, key := range []string{"StandardOutPath", "StandardErrorPath"} {
		out.WriteString("<key>" + key + "</key><string>")
		_ = xml.EscapeText(&out, []byte(logPath))
		out.WriteString("</string>")
	}
	out.WriteString("</dict></plist>")
	return out.String()
}

func verifyPeer(conn *net.UnixConn, expectedPID int) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var peerErr error
	if err := raw.Control(func(fd uintptr) {
		credential, err := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if err != nil {
			peerErr = err
			return
		}
		pid, err := unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
		if err != nil {
			peerErr = err
			return
		}
		if credential.Uid != uint32(os.Getuid()) || pid != expectedPID {
			peerErr = fmt.Errorf("peer identity mismatch: uid=%d pid=%d", credential.Uid, pid)
		}
	}); err != nil {
		return err
	}
	return peerErr
}

// TestLaunchd_Helper 仅由本测试的 launchd job 或其受控子进程重入。
func TestLaunchd_Helper(t *testing.T) {
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 {
		return
	}
	args := os.Args[separator+1:]
	if len(args) != 4 {
		t.Fatal("invalid helper arguments")
	}
	role, socket, token := args[0], args[1], args[2]
	ownerPID, err := strconv.Atoi(args[3])
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := conn.SetDeadline(time.Now().Add(3 * probeBudget)); err != nil {
		t.Fatal(err)
	}
	if err := verifyPeer(conn, ownerPID); err != nil {
		t.Fatal(err)
	}
	group, err := unix.Getpgid(0)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(conn).Encode(probeHello{Role: role, Token: token, PID: os.Getpid(), ParentPID: os.Getppid(), Group: group}); err != nil {
		t.Fatal(err)
	}
	if role == "guard" || role == "worker" {
		childRoles := []string{"grandchild"}
		if role == "guard" {
			childRoles = []string{"worker", "escaped"}
		}
		for _, childRole := range childRoles {
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			// 本原型故意由 launchd 验证异常回收；不能用 guard 的 context 取消代替被测机制。
			cmd := exec.CommandContext(context.Background(), executable, "-test.run=^TestLaunchd_Helper$", "--", childRole, socket, token, args[3])
			if childRole == "escaped" {
				cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			// 不建立日志管道；子进程退出由 launchd 接管，原型主动释放本地句柄。
			if err := cmd.Process.Release(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if role == "guard" {
		var one [1]byte
		if _, err := conn.Read(one[:]); !errors.Is(err, io.EOF) {
			t.Fatalf("owner session got %v, want EOF", err)
		}
		return
	}
	if role != "worker" && role != "escaped" && role != "grandchild" {
		t.Fatalf("unknown helper role %q", role)
	}
	decoder := json.NewDecoder(conn)
	for {
		var request string
		if err := decoder.Decode(&request); errors.Is(err, io.EOF) {
			return
		} else if err != nil {
			t.Fatal(err)
		}
		if request != "ping" {
			t.Fatalf("unknown request %q", request)
		}
		if err := json.NewEncoder(conn).Encode("pong"); err != nil {
			t.Fatal(err)
		}
	}
}
