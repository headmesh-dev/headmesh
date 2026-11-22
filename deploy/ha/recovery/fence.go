package recovery

import (
	"context"
	"fmt"
	"maps"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// Primary identifies the original server and its public IPs in the recovery
// project. The caller supplies these pins from deployment configuration.
type Primary struct {
	ServerID   int64
	ServerName string
	IPv4ID     int64
	IPv6ID     int64 // Zero only if the primary has no IPv6 address.
}

// FencePrimary powers off the original VM, records the fence, and detaches its
// public addresses. Call only after independently confirming an outage and
// taking the recovery workflow's exclusive lock. A nil result means the
// provider confirmed the fence at the time of the final readback.
func FencePrimary(ctx context.Context, client *hcloud.Client, primary Primary) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	state, err := readPrimary(ctx, client, primary)
	if err != nil {
		return err
	}
	switch state.server.Status {
	case hcloud.ServerStatusRunning:
		action, _, err := client.Server.Poweroff(ctx, state.server)
		if err != nil {
			return fmt.Errorf("request primary poweroff: %w", err)
		}
		if err := waitForAction(ctx, client, action); err != nil {
			return fmt.Errorf("wait for primary poweroff: %w", err)
		}
		state, err = readPrimary(ctx, client, primary)
		if err != nil {
			return err
		}
	case hcloud.ServerStatusOff:
	default:
		return fmt.Errorf("primary status %q does not permit fencing", state.server.Status)
	}
	if state.server.Status != hcloud.ServerStatusOff {
		return fmt.Errorf("primary is %q after poweroff", state.server.Status)
	}

	if state.server.Labels["fenced"] != "true" {
		labels := make(map[string]string, len(state.server.Labels)+1)
		maps.Copy(labels, state.server.Labels)
		labels["fenced"] = "true"
		if _, _, err := client.Server.Update(ctx, state.server, hcloud.ServerUpdateOpts{Labels: labels}); err != nil {
			return fmt.Errorf("label primary fenced: %w", err)
		}
		state, err = readPrimary(ctx, client, primary)
		if err != nil {
			return err
		}
	}

	for _, id := range []int64{primary.IPv4ID, primary.IPv6ID} {
		if err := state.checkOffAndMarked(); err != nil {
			return err
		}
		if !state.attached(id) {
			continue
		}
		action, _, err := client.PrimaryIP.Unassign(ctx, id)
		if err != nil {
			return fmt.Errorf("request detachment of primary IP %d: %w", id, err)
		}
		if err := waitForAction(ctx, client, action); err != nil {
			return fmt.Errorf("wait for detachment of primary IP %d: %w", id, err)
		}
		state, err = readPrimary(ctx, client, primary)
		if err != nil {
			return err
		}
		if state.attached(id) {
			return fmt.Errorf("primary IP %d remains attached", id)
		}
	}
	return state.checkFenced()
}

// VerifyFence checks the provider again without making any changes.
func VerifyFence(ctx context.Context, client *hcloud.Client, primary Primary) error {
	state, err := readPrimary(ctx, client, primary)
	if err != nil {
		return err
	}
	return state.checkFenced()
}

type primaryState struct {
	server *hcloud.Server
}

func (s primaryState) attached(id int64) bool {
	return id != 0 && (s.server.PublicNet.IPv4.ID == id || s.server.PublicNet.IPv6.ID == id)
}

func (s primaryState) checkOffAndMarked() error {
	if s.server.Status != hcloud.ServerStatusOff || s.server.Labels["fenced"] != "true" {
		return fmt.Errorf("the primary must be off and labelled fenced=true")
	}
	return nil
}

func (s primaryState) checkFenced() error {
	if err := s.checkOffAndMarked(); err != nil {
		return err
	}
	if s.server.PublicNet.IPv4.ID != 0 || s.server.PublicNet.IPv6.ID != 0 {
		return fmt.Errorf("the fenced primary still has a public IP")
	}
	return nil
}

func readPrimary(ctx context.Context, client *hcloud.Client, primary Primary) (primaryState, error) {
	if primary.ServerID <= 0 || primary.ServerName == "" || primary.IPv4ID <= 0 ||
		primary.IPv6ID < 0 || primary.IPv4ID == primary.IPv6ID {
		return primaryState{}, fmt.Errorf("invalid recovery primary pins")
	}
	server, _, err := client.Server.GetByID(ctx, primary.ServerID)
	if err != nil {
		return primaryState{}, fmt.Errorf("read primary server: %w", err)
	}
	if server == nil || server.ID != primary.ServerID || server.Name != primary.ServerName {
		return primaryState{}, fmt.Errorf("provider server does not match the pinned primary")
	}
	if len(server.PublicNet.FloatingIPs) != 0 || len(server.PrivateNet) != 0 {
		return primaryState{}, fmt.Errorf("primary has an unexpected floating IP or private network")
	}

	state := primaryState{server: server}
	for _, address := range []struct {
		family      hcloud.PrimaryIPType
		expectedID  int64
		attachedID  int64
		unspecified bool
	}{
		{hcloud.PrimaryIPTypeIPv4, primary.IPv4ID, server.PublicNet.IPv4.ID, server.PublicNet.IPv4.IsUnspecified()},
		{hcloud.PrimaryIPTypeIPv6, primary.IPv6ID, server.PublicNet.IPv6.ID, server.PublicNet.IPv6.IsUnspecified()},
	} {
		if (address.attachedID != 0 && address.attachedID != address.expectedID) ||
			(address.attachedID == 0) != address.unspecified {
			return primaryState{}, fmt.Errorf("primary server %s does not match the configured network", address.family)
		}
		if address.expectedID == 0 {
			continue
		}
		ip, _, err := client.PrimaryIP.GetByID(ctx, address.expectedID)
		if err != nil {
			return primaryState{}, fmt.Errorf("read primary IP %d: %w", address.expectedID, err)
		}
		if ip == nil || ip.ID != address.expectedID || ip.Type != address.family {
			return primaryState{}, fmt.Errorf("provider primary IP does not match pinned %s %d", address.family, address.expectedID)
		}
		if address.attachedID != 0 {
			if ip.AssigneeID != primary.ServerID || ip.AssigneeType != "server" {
				return primaryState{}, fmt.Errorf("primary IP %d assignment disagrees with the server record", ip.ID)
			}
		} else if ip.AssigneeID != 0 || ip.AssigneeType != "unassigned" || state.checkOffAndMarked() != nil {
			return primaryState{}, fmt.Errorf("primary IP %d is detached without a confirmed fence or reports another assignee", ip.ID)
		}
	}
	return state, nil
}

func waitForAction(ctx context.Context, client *hcloud.Client, action *hcloud.Action) error {
	if action == nil || action.ID <= 0 {
		return fmt.Errorf("provider did not return a valid action")
	}
	err := client.Action.WaitForFunc(ctx, func(update *hcloud.Action) error {
		if update.ID != action.ID {
			return fmt.Errorf("provider returned an unexpected action ID")
		}
		switch update.Status {
		case hcloud.ActionStatusRunning, hcloud.ActionStatusSuccess:
			return nil
		case hcloud.ActionStatusError:
			return update.Error()
		default:
			return fmt.Errorf("provider returned action status %q", update.Status)
		}
	}, action)
	if err != nil {
		return fmt.Errorf("poll action %d: %w", action.ID, err)
	}
	return nil
}
