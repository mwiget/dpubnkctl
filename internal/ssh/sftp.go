package ssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/pkg/sftp"
)

// PushFile copies localPath to remotePath via SFTP over the existing
// SSH connection. Reports progress every progressEvery bytes via cb if
// non-nil. Overwrites the remote file if it exists.
//
// Designed for the BFB image push (~2 GB) — uses an 8 MB buffer for
// throughput on local-LAN scenarios.
func (c *Client) PushFile(ctx context.Context, localPath, remotePath string, progress func(written, total int64)) error {
	src, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open local %s: %w", localPath, err)
	}
	defer src.Close()
	stat, err := src.Stat()
	if err != nil {
		return err
	}
	total := stat.Size()

	client, err := sftp.NewClient(c.conn)
	if err != nil {
		return fmt.Errorf("sftp init: %w", err)
	}
	defer client.Close()

	dst, err := client.Create(remotePath)
	if err != nil {
		return fmt.Errorf("sftp create %s: %w", remotePath, err)
	}
	defer dst.Close()

	buf := make([]byte, 8*1024*1024)
	var written int64
	const reportEvery = 64 * 1024 * 1024 // 64 MiB
	nextReport := int64(reportEvery)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return fmt.Errorf("sftp write: %w", werr)
			}
			written += int64(n)
			if progress != nil && written >= nextReport {
				progress(written, total)
				nextReport += reportEvery
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fmt.Errorf("read local: %w", rerr)
		}
	}
	if progress != nil {
		progress(written, total)
	}
	return nil
}

// RemoteStat returns the size and modification time of the file at
// remotePath via SFTP, or an error wrapping os.ErrNotExist if the file
// does not exist. Used to detect pre-staged BFBs (provisioning.bfb_on_host)
// so provision_dpu can skip the SFTP upload.
func (c *Client) RemoteStat(ctx context.Context, remotePath string) (int64, error) {
	client, err := sftp.NewClient(c.conn)
	if err != nil {
		return 0, fmt.Errorf("sftp init: %w", err)
	}
	defer client.Close()
	st, err := client.Stat(remotePath)
	if err != nil {
		// pkg/sftp surfaces SSH_FX_NO_SUCH_FILE as an error whose chain
		// includes os.ErrNotExist; wrap with the sentinel for callers.
		if errors.Is(err, os.ErrNotExist) {
			return 0, fmt.Errorf("stat %s: %w", remotePath, os.ErrNotExist)
		}
		return 0, fmt.Errorf("stat %s: %w", remotePath, err)
	}
	_ = ctx
	return st.Size(), nil
}

// PushBytes writes data to remotePath via SFTP. Convenience wrapper for
// small files (rendered configs, scripts) that don't warrant a tempfile.
func (c *Client) PushBytes(ctx context.Context, data []byte, remotePath string) error {
	client, err := sftp.NewClient(c.conn)
	if err != nil {
		return fmt.Errorf("sftp init: %w", err)
	}
	defer client.Close()
	dst, err := client.Create(remotePath)
	if err != nil {
		return fmt.Errorf("sftp create %s: %w", remotePath, err)
	}
	defer dst.Close()
	if _, err := io.Copy(dst, bytes.NewReader(data)); err != nil {
		return fmt.Errorf("sftp write %s: %w", remotePath, err)
	}
	_ = ctx // ctx reserved for future cancel support
	return nil
}
