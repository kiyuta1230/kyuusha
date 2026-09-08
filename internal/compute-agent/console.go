package computeagent

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/nats-io/nats.go"

	"gitlab.com/ki.yuta1230/kyuusha/internal/compute"
)

const (
	// defaultConsoleTailBytes is used when a request's TailBytes is 0.
	defaultConsoleTailBytes = 64 * 1024
	consoleChunkSize        = 32 * 1024
	consolePollInterval     = 500 * time.Millisecond
	// consoleFollowMaxDuration bounds a follow session in case its stop
	// signal (published by the Reconciler when the client disconnects) is
	// ever lost -- see docs/specs/firecracker-boot.md.
	consoleFollowMaxDuration = 30 * time.Minute
)

// handleConsoleRequest is the NATS core (not JetStream -- see
// compute.ConsoleRequestSubject) subscription callback for this
// hypervisor's console requests. Handed off to a goroutine immediately: a
// Follow request can run for a long time, and must not block this agent's
// single console subscription from accepting the next request.
func (a *Agent) handleConsoleRequest(msg *nats.Msg) {
	var req compute.ConsoleRequest
	if err := json.Unmarshal(msg.Data, &req); err != nil {
		slog.Error("compute-agent: bad console request", "err", err)
		return
	}
	go a.serveConsole(req)
}

func (a *Agent) serveConsole(req compute.ConsoleRequest) {
	path, ok := a.consoleLogPath(req.VMID)
	if !ok {
		a.publishConsoleDone(req.ReplySubject, "no console log for this vm_id")
		return
	}
	data, offset, err := readTail(path, req.TailBytes)
	if err != nil {
		a.publishConsoleDone(req.ReplySubject, "no console log for this vm_id: "+err.Error())
		return
	}

	// Ack immediately, even with nothing to say yet (data may be empty) --
	// see compute.ConsoleRequest's doc: the Reconciler's relay has a
	// bounded wait for this first reply, which must not depend on there
	// being history to send or Follow ever producing new output.
	if err := a.NC.Publish(req.ReplySubject, nil); err != nil {
		return
	}

	for i := 0; i < len(data); i += consoleChunkSize {
		end := min(i+consoleChunkSize, len(data))
		if err := a.NC.Publish(req.ReplySubject, data[i:end]); err != nil {
			return
		}
	}

	if !req.Follow {
		a.publishConsoleDone(req.ReplySubject, "")
		return
	}
	a.followConsole(req, path, offset)
}

// consoleLogPath finds which of a.Drivers actually booted vmID, by
// checking which driver's ConsoleLogPath resolves to a file that actually
// exists on disk. handleCreate doesn't separately record which driver won
// a given VM -- a VM's Image format determines which drivers can even
// consume it (see internal/compute/image.go's validateImage), so at most
// one driver would ever have booted it for real, making "the first
// existing file" unambiguous in practice.
func (a *Agent) consoleLogPath(vmID string) (string, bool) {
	for _, d := range a.Drivers {
		path := d.ConsoleLogPath(vmID)
		if _, err := os.Stat(path); err == nil {
			return path, true
		}
	}
	return "", false
}

func (a *Agent) followConsole(req compute.ConsoleRequest, path string, offset int64) {
	stopSub, err := a.NC.SubscribeSync(req.ReplySubject + ".stop")
	if err != nil {
		a.publishConsoleDone(req.ReplySubject, err.Error())
		return
	}
	defer stopSub.Unsubscribe()

	deadline := time.Now().Add(consoleFollowMaxDuration)
	for time.Now().Before(deadline) {
		// Doubles as both the cancellation check and the poll interval:
		// a timeout here just means "no stop signal yet, keep going".
		if _, err := stopSub.NextMsg(consolePollInterval); err == nil {
			a.publishConsoleDone(req.ReplySubject, "")
			return
		}

		buf, newOffset, err := readFrom(path, offset)
		if err != nil {
			a.publishConsoleDone(req.ReplySubject, "console log disappeared: "+err.Error())
			return
		}
		offset = newOffset
		if len(buf) == 0 {
			continue
		}
		if err := a.NC.Publish(req.ReplySubject, buf); err != nil {
			return
		}
	}
	a.publishConsoleDone(req.ReplySubject, "")
}

func (a *Agent) publishConsoleDone(replySubject, errMsg string) {
	msg := nats.NewMsg(replySubject)
	msg.Header.Set(compute.ConsoleDoneHeader, "true")
	if errMsg != "" {
		msg.Header.Set(compute.ConsoleErrorHeader, errMsg)
	}
	_ = a.NC.PublishMsg(msg)
}

// readTail returns the last tailBytes of path (its whole content if
// tailBytes < 0, or defaultConsoleTailBytes if tailBytes == 0), plus the
// file's size at read time (the offset a subsequent readFrom should resume
// from for a Follow session).
func readTail(path string, tailBytes int64) (data []byte, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	size = stat.Size()

	n := tailBytes
	if n == 0 {
		n = defaultConsoleTailBytes
	}
	var start int64
	if n > 0 && size > n {
		start = size - n
	}
	if start > 0 {
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			return nil, 0, err
		}
	}
	data, err = io.ReadAll(f)
	if err != nil {
		return nil, 0, err
	}
	return data, size, nil
}

// readFrom reads whatever has been appended to path since offset, and
// returns the new offset to resume from next time.
func readFrom(path string, offset int64) (data []byte, newOffset int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, offset, err
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return nil, offset, err
	}
	if stat.Size() <= offset {
		return nil, offset, nil
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, err
	}
	data, err = io.ReadAll(f)
	if err != nil {
		return nil, offset, err
	}
	return data, offset + int64(len(data)), nil
}
