package main

import (
	"fmt"
	"strconv"
	"strings"
)

func (s *Service) ensureDockerNetwork() error {
	networkCfg := s.cfg.Docker.Network
	name := strings.TrimSpace(networkCfg.Name)
	if name == "" {
		return nil
	}

	driver := strings.TrimSpace(networkCfg.Driver)
	if driver == "" {
		driver = defaultDockerNetworkDriver
	}
	enableICC := true
	if networkCfg.EnableICC != nil {
		enableICC = *networkCfg.EnableICC
	}

	if _, err := runCommand("docker", "network", "inspect", name); err == nil {
		bootInfo(
			"configured docker network name=%s mode=%s driver=%s interface=%s mtu=%d icc=%t ipv6=%t internal=%t attachable=%t dns=%v",
			name,
			networkCfg.Mode,
			driver,
			networkCfg.Interface,
			networkCfg.NetworkMTU,
			enableICC,
			networkCfg.EnableIPv6,
			networkCfg.IsInternal,
			networkCfg.Attachable,
			networkCfg.DNS,
		)
		return nil
	}

	args := s.buildDockerNetworkCreateArgs(name, driver, enableICC, true)
	networkID, err := runCommand("docker", args...)
	if err != nil && isDockerPoolOverlapError(err) {
		bootWarn(
			"docker network subnet overlap detected for name=%s v4_subnet=%s v6_subnet=%s; retrying with automatic IPAM",
			name,
			networkCfg.Interfaces.V4.Subnet,
			networkCfg.Interfaces.V6.Subnet,
		)
		args = s.buildDockerNetworkCreateArgs(name, driver, enableICC, false)
		networkID, err = runCommand("docker", args...)
	}
	if err != nil {
		if isDockerAlreadyExistsError(err) {
			bootWarn("docker network already exists name=%s", name)
			return nil
		}
		return fmt.Errorf("create docker network %s failed: %w", name, err)
	}

	bootInfo(
		"created docker network name=%s id=%s mode=%s driver=%s interface=%s mtu=%d icc=%t ipv6=%t internal=%t attachable=%t dns=%v",
		name,
		strings.TrimSpace(networkID),
		networkCfg.Mode,
		driver,
		networkCfg.Interface,
		networkCfg.NetworkMTU,
		enableICC,
		networkCfg.EnableIPv6,
		networkCfg.IsInternal,
		networkCfg.Attachable,
		networkCfg.DNS,
	)
	return nil
}

func (s *Service) buildDockerNetworkCreateArgs(name, driver string, enableICC bool, withIPAM bool) []string {
	networkCfg := s.cfg.Docker.Network
	args := []string{"network", "create", "--driver", driver}
	args = append(args, "--opt", "com.docker.network.bridge.enable_icc="+strconv.FormatBool(enableICC))
	if networkCfg.NetworkMTU > 0 {
		args = append(args, "--opt", "com.docker.network.driver.mtu="+strconv.FormatInt(networkCfg.NetworkMTU, 10))
	}
	if networkCfg.IsInternal {
		args = append(args, "--internal")
	}
	if networkCfg.Attachable {
		args = append(args, "--attachable")
	}
	if withIPAM {
		if networkCfg.Interfaces.V4.Subnet != "" {
			args = append(args, "--subnet", networkCfg.Interfaces.V4.Subnet)
		}
		if networkCfg.Interfaces.V4.Gateway != "" {
			args = append(args, "--gateway", networkCfg.Interfaces.V4.Gateway)
		}
	}
	if networkCfg.EnableIPv6 {
		args = append(args, "--ipv6")
		if withIPAM {
			if networkCfg.Interfaces.V6.Subnet != "" {
				args = append(args, "--subnet", networkCfg.Interfaces.V6.Subnet)
			}
			if networkCfg.Interfaces.V6.Gateway != "" {
				args = append(args, "--gateway", networkCfg.Interfaces.V6.Gateway)
			}
		}
	}
	args = append(args, name)
	return args
}

func isDockerAlreadyExistsError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already exists")
}

func isDockerPoolOverlapError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "pool overlaps with other one on this address space")
}
