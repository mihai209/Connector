package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

var ErrAttachedStreamUnavailable = errors.New("attached stream unavailable")

func buildContainerInputPayload(command string) []byte {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil
	}

	// Match old connector behavior for control characters like ^C.
	if len(command) == 2 && strings.HasPrefix(command, "^") {
		ctrl := command[1]
		if ctrl >= '@' && ctrl <= '_' {
			return []byte{ctrl - 64}
		}
		if ctrl >= 'a' && ctrl <= 'z' {
			return []byte{ctrl - 96}
		}
	}

	return []byte(command + "\n")
}

func sendCommandToContainerStdin(containerName, command string) error {
	payload := buildContainerInputPayload(command)
	if len(payload) == 0 {
		return fmt.Errorf("empty command payload")
	}

	return sendPayloadToContainerStdin(containerName, payload)
}

func (s *Service) sendCommandToServerStdin(serverID int, containerName, command string) error {
	payload := buildContainerInputPayload(command)
	if len(payload) == 0 {
		return fmt.Errorf("empty command payload")
	}

	if err := s.sendPayloadViaAttachedStream(serverID, payload); err == nil {
		return nil
	}

	return sendPayloadToContainerStdin(containerName, payload)
}

func sendPayloadToContainerStdin(containerName string, payload []byte) error {
	if err := sendCommandViaDockerSocketAttach(containerName, payload); err == nil {
		return nil
	} else {
		// Fallback path for images where docker socket attach fails.
		if procErr := sendCommandViaProcFD0(containerName, payload); procErr == nil {
			return nil
		} else {
			if attachErr := sendCommandViaAttach(containerName, payload); attachErr == nil {
				return nil
			} else {
				return fmt.Errorf("stdin injection failed (socket_attach=%v, procfd0=%v, attach=%v)", err, procErr, attachErr)
			}
		}
	}
}

func (s *Service) sendPayloadViaAttachedStream(serverID int, payload []byte) error {
	s.attachMu.Lock()
	stream := s.attachStdin[serverID]
	s.attachMu.Unlock()
	if stream == nil || stream.Stdin == nil {
		return ErrAttachedStreamUnavailable
	}

	stream.WriteMu.Lock()
	defer stream.WriteMu.Unlock()

	if _, err := stream.Stdin.Write(payload); err != nil {
		s.clearAttachedStream(serverID)
		return err
	}
	return nil
}

func sendCommandViaProcFD0(containerName string, payload []byte) error {
	_, err := runCommandWithInput(
		string(payload),
		"docker",
		"exec",
		"-i",
		"-u",
		"0",
		containerName,
		"/bin/sh",
		"-lc",
		"cat > /proc/1/fd/0",
	)
	return err
}

func sendCommandViaDockerSocketAttach(containerName string, payload []byte) error {
	conn, err := net.DialTimeout("unix", "/var/run/docker.sock", 2*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	target := "/containers/" + url.PathEscape(containerName) + "/attach?stream=1&stdin=1&stdout=0&stderr=0"
	request := "POST " + target + " HTTP/1.1\r\n" +
		"Host: docker\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: tcp\r\n" +
		"Content-Length: 0\r\n\r\n"

	if _, err := conn.Write([]byte(request)); err != nil {
		return err
	}

	reader := bufio.NewReader(conn)
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		return err
	}

	statusCode, parseErr := parseHTTPStatusCode(statusLine)
	if parseErr != nil {
		return parseErr
	}

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		if line == "\r\n" {
			break
		}
	}

	if statusCode != 101 && statusCode != 200 {
		return fmt.Errorf("docker attach unexpected status: %s", strings.TrimSpace(statusLine))
	}

	if _, err := conn.Write(payload); err != nil {
		return err
	}
	return nil
}

func parseHTTPStatusCode(statusLine string) (int, error) {
	parts := strings.Fields(strings.TrimSpace(statusLine))
	if len(parts) < 2 {
		return 0, fmt.Errorf("invalid docker http status line: %q", statusLine)
	}
	code, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, fmt.Errorf("invalid docker http status code in %q: %w", statusLine, err)
	}
	return code, nil
}

func sendCommandViaAttach(containerName string, payload []byte) error {
	cmd := exec.Command("docker", "attach", containerName)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	_, writeErr := stdin.Write(payload)
	closeErr := stdin.Close()

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	timedOut := false
	var waitErr error
	select {
	case <-time.After(2 * time.Second):
		timedOut = true
		_ = cmd.Process.Kill()
		waitErr = <-waitCh
	case waitErr = <-waitCh:
	}

	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if timedOut {
		// Expected for attach mode: we intentionally kill the local attach process after stdin is sent.
		return nil
	}
	if waitErr != nil {
		return waitErr
	}
	return nil
}
