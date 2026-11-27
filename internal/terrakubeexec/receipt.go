package terrakubeexec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
)

const receiptPrefix = "TERRAKUBE_DEPLOYMENT_RESULT "

type deploymentReceipt struct {
	Version     int    `json:"version"`
	JobID       string `json:"job_id"`
	WorkspaceID string `json:"workspace_id"`
	CommitID    string `json:"commit_id"`
	Outcome     string `json:"outcome"`
}

var (
	jobIDPattern       = regexp.MustCompile(`^[1-9][0-9]*$`)
	workspaceIDPattern = regexp.MustCompile(`^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)
)

func (r deploymentReceipt) write(output io.Writer) error {
	body, err := json.Marshal(r)
	if err != nil {
		return executionError("encode the Terrakube deployment result", err)
	}

	if _, err := fmt.Fprintf(output, "%s%s\n", receiptPrefix, body); err != nil {
		return executionError("write the Terrakube deployment result", err)
	}

	return nil
}

type subprocessOutput struct {
	destination io.Writer
	midline     bool
}

func (output *subprocessOutput) Write(data []byte) (int, error) {
	var written int

	for len(data) != 0 {
		end := bytes.IndexAny(data, "\r\n")
		length := len(data)
		if end >= 0 {
			length = end + 1
		}

		if !output.midline && end != 0 {
			if _, err := io.WriteString(output.destination, "| "); err != nil {
				return written, fmt.Errorf("write subprocess output: %w", err)
			}
		}

		count, err := output.destination.Write(data[:length])
		written += count
		if err != nil {
			return written, fmt.Errorf("write subprocess output: %w", err)
		}

		if count != length {
			return written, io.ErrShortWrite
		}

		output.midline = end < 0
		data = data[length:]
	}

	return written, nil
}

func (output *subprocessOutput) finish() error {
	if !output.midline {
		return nil
	}

	if _, err := io.WriteString(output.destination, "\n"); err != nil {
		return executionError("finish subprocess output", err)
	}

	return nil
}
