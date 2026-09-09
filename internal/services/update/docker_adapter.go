// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (c) 2024-2026 usulnet contributors
// https://github.com/fr4nsys/usulnet

package update

import (
	"context"
	"fmt"
	"sort"
	"strings"

	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/google/uuid"

	dockerpkg "github.com/fr4nsys/usulnet/internal/docker"
	"github.com/fr4nsys/usulnet/internal/pkg/errors"
)

// HostClientProvider provides Docker clients for a given host.
type HostClientProvider interface {
	GetClient(ctx context.Context, hostID uuid.UUID) (dockerpkg.ClientAPI, error)
}

// DockerClientAdapter adapts a host-based Docker client pool to the DockerClient
// interface required by the update service. It resolves the client lazily on each call.
type DockerClientAdapter struct {
	provider HostClientProvider
	hostID   uuid.UUID
}

// NewDockerClientAdapter wraps a host client provider to implement DockerClient.
func NewDockerClientAdapter(provider HostClientProvider, hostID uuid.UUID) *DockerClientAdapter {
	return &DockerClientAdapter{provider: provider, hostID: hostID}
}

func (a *DockerClientAdapter) getClient(ctx context.Context) (dockerpkg.ClientAPI, error) {
	return a.provider.GetClient(ctx, a.hostID)
}

func (a *DockerClientAdapter) ContainerInspect(ctx context.Context, containerID string) (*dockertypes.ContainerJSON, error) {
	c, err := a.getClient(ctx)
	if err != nil {
		return nil, err
	}
	inspect, err := c.ContainerInspectRaw(ctx, containerID)
	if err != nil {
		return nil, err
	}
	return &inspect, nil
}

func (a *DockerClientAdapter) ContainerStop(ctx context.Context, containerID string, timeout *int) error {
	c, err := a.getClient(ctx)
	if err != nil {
		return err
	}
	return c.ContainerStop(ctx, containerID, timeout)
}

func (a *DockerClientAdapter) ContainerStart(ctx context.Context, containerID string) error {
	c, err := a.getClient(ctx)
	if err != nil {
		return err
	}
	return c.ContainerStart(ctx, containerID)
}

func (a *DockerClientAdapter) ContainerRemove(ctx context.Context, containerID string, force bool) error {
	c, err := a.getClient(ctx)
	if err != nil {
		return err
	}
	return c.ContainerRemove(ctx, containerID, force, false)
}

func (a *DockerClientAdapter) ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, name string) (string, error) {
	c, err := a.getClient(ctx)
	if err != nil {
		return "", err
	}
	// Raw container creation requires a direct Docker client
	directClient, ok := c.(*dockerpkg.Client)
	if !ok {
		return "", errors.New(errors.CodeDockerConnection, "container creation with raw SDK types not supported for remote hosts")
	}
	cli := directClient.Raw()
	if cli == nil {
		return "", errors.New(errors.CodeDockerConnection, "docker client is closed")
	}

	// Older daemons accept only one endpoint at creation time, so create with
	// the primary network and connect the remaining ones before start.
	primary, extra := splitEndpoints(networkingConfig, hostConfig)
	resp, err := cli.ContainerCreate(ctx, config, hostConfig, primary, nil, name)
	if err != nil {
		return "", err
	}
	for netName, ep := range extra {
		if err := cli.NetworkConnect(ctx, netName, resp.ID, ep); err != nil {
			_ = cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
			return "", fmt.Errorf("connect network %q: %w", netName, err)
		}
	}
	return resp.ID, nil
}

// splitEndpoints picks the endpoint to pass at creation time and returns the
// rest to be connected afterwards. The primary is the network named by
// HostConfig.NetworkMode when it is one of the attachments, otherwise the
// first network by name so the choice is deterministic.
func splitEndpoints(nc *network.NetworkingConfig, hc *container.HostConfig) (*network.NetworkingConfig, map[string]*network.EndpointSettings) {
	if nc == nil || len(nc.EndpointsConfig) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(nc.EndpointsConfig))
	for n := range nc.EndpointsConfig {
		names = append(names, n)
	}
	sort.Strings(names)
	primary := names[0]
	if hc != nil {
		if mode := string(hc.NetworkMode); mode != "" {
			if _, ok := nc.EndpointsConfig[mode]; ok {
				primary = mode
			}
		}
	}
	extra := make(map[string]*network.EndpointSettings, len(names)-1)
	for _, n := range names {
		if n != primary {
			extra[n] = nc.EndpointsConfig[n]
		}
	}
	return &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{primary: nc.EndpointsConfig[primary]}}, extra
}

func (a *DockerClientAdapter) ContainerRename(ctx context.Context, containerID, newName string) error {
	c, err := a.getClient(ctx)
	if err != nil {
		return err
	}
	return c.ContainerRename(ctx, containerID, newName)
}

func (a *DockerClientAdapter) ContainerList(ctx context.Context) ([]ContainerInfo, error) {
	c, err := a.getClient(ctx)
	if err != nil {
		return nil, err
	}
	containers, err := c.ContainerList(ctx, dockerpkg.ContainerListOptions{All: true})
	if err != nil {
		return nil, err
	}
	result := make([]ContainerInfo, 0, len(containers))
	for _, ct := range containers {
		// ImageID contains the image digest (sha256:...) which enables
		// accurate update detection for "latest" tagged containers.
		digest := ct.ImageID
		if digest != "" && !strings.HasPrefix(digest, "sha256:") {
			digest = ""
		}
		result = append(result, ContainerInfo{
			ID:     ct.ID,
			Name:   ct.Name,
			Image:  ct.Image,
			Digest: digest,
		})
	}
	return result, nil
}

func (a *DockerClientAdapter) ImagePull(ctx context.Context, ref string, onProgress func(status string)) error {
	c, err := a.getClient(ctx)
	if err != nil {
		return err
	}
	progressCh, err := c.ImagePull(ctx, ref, dockerpkg.ImagePullOptions{})
	if err != nil {
		return err
	}
	for p := range progressCh {
		if onProgress != nil {
			onProgress(p.Status)
		}
	}
	return nil
}

func (a *DockerClientAdapter) ImageInspect(ctx context.Context, imageID string) (*ImageInfo, error) {
	c, err := a.getClient(ctx)
	if err != nil {
		return nil, err
	}
	details, err := c.ImageGet(ctx, imageID)
	if err != nil {
		return nil, err
	}
	return &ImageInfo{
		ID:          details.ID,
		RepoTags:    details.RepoTags,
		RepoDigests: details.RepoDigests,
		Created:     details.Created,
		Size:        details.Size,
		Labels:      details.Labels,
	}, nil
}
