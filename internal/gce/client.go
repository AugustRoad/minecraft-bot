package gce

import (
	"context"
	"fmt"

	compute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
)

// GCE VM lifecycle statuses returned by Compute Engine.
const (
	StatusProvisioning = "PROVISIONING"
	StatusStaging      = "STAGING"
	StatusRunning      = "RUNNING"
	StatusStopping     = "STOPPING"
	StatusSuspending   = "SUSPENDING"
	StatusSuspended    = "SUSPENDED"
	StatusRepairing    = "REPAIRING"
	StatusTerminated   = "TERMINATED"
)

// InstanceInfo describes the current power state and IP addresses of the Minecraft VM.
type InstanceInfo struct {
	Name       string
	Status     string
	ExternalIP string
	InternalIP string
}

// IsRunning reports whether the VM is in the RUNNING state.
func (i InstanceInfo) IsRunning() bool {
	return i.Status == StatusRunning
}

// IsStopped reports whether the VM is fully powered off.
func (i InstanceInfo) IsStopped() bool {
	return i.Status == StatusTerminated || i.Status == StatusSuspended
}

// VMManager abstracts GCE instance operations for testing and dependency injection.
type VMManager interface {
	GetStatus(ctx context.Context) (InstanceInfo, error)
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Close() error
}

// Client wraps the Google Cloud Compute Engine Instances REST client.
type Client struct {
	instances    *compute.InstancesClient
	project      string
	zone         string
	instanceName string
}

// NewClient initializes a GCE Compute Engine client using Application Default Credentials
// (on a GCE VM, this automatically authenticates via the attached Service Account).
func NewClient(ctx context.Context, project, zone, instanceName string) (*Client, error) {
	instances, err := compute.NewInstancesRESTClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("create compute instances client: %w", err)
	}
	return &Client{
		instances:    instances,
		project:      project,
		zone:         zone,
		instanceName: instanceName,
	}, nil
}

// GetStatus fetches the current status, ephemeral external IP (natIP), and VPC internal IP.
func (c *Client) GetStatus(ctx context.Context) (InstanceInfo, error) {
	inst, err := c.instances.Get(ctx, &computepb.GetInstanceRequest{
		Project:  c.project,
		Zone:     c.zone,
		Instance: c.instanceName,
	})
	if err != nil {
		return InstanceInfo{}, fmt.Errorf("get gce instance %q: %w", c.instanceName, err)
	}

	info := InstanceInfo{
		Name:   inst.GetName(),
		Status: inst.GetStatus(),
	}

	for _, nic := range inst.GetNetworkInterfaces() {
		if info.InternalIP == "" && nic.GetNetworkIP() != "" {
			info.InternalIP = nic.GetNetworkIP()
		}
		for _, ac := range nic.GetAccessConfigs() {
			if info.ExternalIP == "" && ac.GetNatIP() != "" {
				info.ExternalIP = ac.GetNatIP()
			}
		}
	}

	return info, nil
}

// Start sends a start request to GCE and waits for the operation to complete.
func (c *Client) Start(ctx context.Context) error {
	op, err := c.instances.Start(ctx, &computepb.StartInstanceRequest{
		Project:  c.project,
		Zone:     c.zone,
		Instance: c.instanceName,
	})
	if err != nil {
		return fmt.Errorf("start gce instance %q: %w", c.instanceName, err)
	}
	if err := op.Wait(ctx); err != nil {
		return fmt.Errorf("wait for start operation on %q: %w", c.instanceName, err)
	}
	return nil
}

// Stop sends a stop request to GCE and waits for the instance to terminate.
func (c *Client) Stop(ctx context.Context) error {
	op, err := c.instances.Stop(ctx, &computepb.StopInstanceRequest{
		Project:  c.project,
		Zone:     c.zone,
		Instance: c.instanceName,
	})
	if err != nil {
		return fmt.Errorf("stop gce instance %q: %w", c.instanceName, err)
	}
	if err := op.Wait(ctx); err != nil {
		return fmt.Errorf("wait for stop operation on %q: %w", c.instanceName, err)
	}
	return nil
}

// Close releases underlying HTTP/gRPC connections.
func (c *Client) Close() error {
	if c.instances == nil {
		return nil
	}
	return c.instances.Close()
}
