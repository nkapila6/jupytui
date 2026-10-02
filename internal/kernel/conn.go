package kernel

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// ConnInfo is the kernel connection file format.
type ConnInfo struct {
	IP              string `json:"ip"`
	Transport       string `json:"transport"`
	ShellPort       int    `json:"shell_port"`
	IOPubPort       int    `json:"iopub_port"`
	StdinPort       int    `json:"stdin_port"`
	ControlPort     int    `json:"control_port"`
	HBPort          int    `json:"hb_port"`
	Key             string `json:"key"`
	SignatureScheme string `json:"signature_scheme"`
	KernelName      string `json:"kernel_name"`
}

func (c ConnInfo) addr(port int) string {
	return fmt.Sprintf("%s://%s:%d", c.Transport, c.IP, port)
}

func newConnInfo() (ConnInfo, error) {
	ports, err := freePorts(5)
	if err != nil {
		return ConnInfo{}, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return ConnInfo{}, err
	}
	return ConnInfo{
		IP:              "127.0.0.1",
		Transport:       "tcp",
		ShellPort:       ports[0],
		IOPubPort:       ports[1],
		StdinPort:       ports[2],
		ControlPort:     ports[3],
		HBPort:          ports[4],
		Key:             hex.EncodeToString(key),
		SignatureScheme: "hmac-sha256",
	}, nil
}

// writeConnFile puts the connection file in a private temp dir since
// the key in it lets anyone run code in the kernel.
func writeConnFile(c ConnInfo) (string, error) {
	dir, err := os.MkdirTemp("", "jupytui-")
	if err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "kernel.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// freePorts holds all listeners open until every port is picked so we
// don't get the same port twice. There's still a small race before the
// kernel binds, same as jupyter_client.
func freePorts(n int) ([]int, error) {
	var ls []net.Listener
	defer func() {
		for _, l := range ls {
			l.Close()
		}
	}()
	ports := make([]int, 0, n)
	for range n {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		ls = append(ls, l)
		ports = append(ports, l.Addr().(*net.TCPAddr).Port)
	}
	return ports, nil
}
