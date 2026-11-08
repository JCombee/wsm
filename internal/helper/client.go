package helper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// Client sends requests to wsm-helper.
type Client struct {
	Socket string
}

// Do sends one request and waits for the answer.
func (c *Client) Do(ctx context.Context, req Request) error {
	if err := req.Validate(); err != nil {
		return fmt.Errorf("helper: %w", err)
	}
	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("helper: %w", err)
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return fmt.Errorf("helper: %w", err)
	}
	defer conn.Close()
	return exchange(ctx, conn.(*net.UnixConn), req, data)
}

// exchange sends one encoded request on uc and reads the response.
func exchange(ctx context.Context, uc *net.UnixConn, req Request, data []byte) error {
	if deadline, ok := ctx.Deadline(); ok {
		uc.SetDeadline(deadline)
	} else {
		uc.SetDeadline(time.Now().Add(5 * time.Minute))
	}
	stop := context.AfterFunc(ctx, func() { uc.SetDeadline(time.Now()) })
	defer stop()
	if _, err := uc.Write(data); err != nil {
		return fmt.Errorf("helper %s: write: %w", req.Type, err)
	}
	if err := uc.CloseWrite(); err != nil {
		return fmt.Errorf("helper %s: %w", req.Type, err)
	}
	raw, err := io.ReadAll(io.LimitReader(uc, MaxMessage+1))
	if err != nil {
		return fmt.Errorf("helper %s: read: %w", req.Type, err)
	}
	if len(raw) > MaxMessage {
		return fmt.Errorf("helper %s: response exceeds %d bytes", req.Type, MaxMessage)
	}
	if len(raw) == 0 {
		return fmt.Errorf("helper %s: connection closed without a response", req.Type)
	}
	var resp Response
	if err := decodeStrict(raw, &resp); err != nil {
		return fmt.Errorf("helper %s: decode response: %w", req.Type, err)
	}
	if !resp.OK {
		if resp.Error == "" {
			resp.Error = "failed"
		}
		return fmt.Errorf("helper %s: %w", req.Type, errors.New(resp.Error))
	}
	return nil
}

// CreateProjectUser asks the helper to create the user for uid.
func (c *Client) CreateProjectUser(ctx context.Context, uid int) error {
	return c.Do(ctx, Request{Type: TypeCreateProjectUser, UID: &uid})
}

// DeleteProjectUser asks the helper to remove the user for uid and its files.
func (c *Client) DeleteProjectUser(ctx context.Context, uid int) error {
	return c.Do(ctx, Request{Type: TypeDeleteProjectUser, UID: &uid})
}

// SetupWSM0 asks the helper to create the wsm0 interface.
func (c *Client) SetupWSM0(ctx context.Context) error {
	return c.Do(ctx, Request{Type: TypeSetupWSM0})
}

// ApplyBaseRules asks the helper to load the base nftables rules.
func (c *Client) ApplyBaseRules(ctx context.Context) error {
	return c.Do(ctx, Request{Type: TypeApplyBaseRules})
}
