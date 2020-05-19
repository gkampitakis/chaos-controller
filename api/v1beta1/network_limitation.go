// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2020 Datadog, Inc.

package v1beta1

import (
	"strconv"

	chaostypes "github.com/DataDog/chaos-controller/types"
	"k8s.io/apimachinery/pkg/types"
)

// NetworkLimitationSpec represents a network bandwidth limitation injection
type NetworkLimitationSpec struct {
	BytesPerSec uint `json:"bytesPerSec"`
}

// GenerateArgs generates injection or cleanup pod arguments for the given spec
func (s *NetworkLimitationSpec) GenerateArgs(mode chaostypes.PodMode, uid types.UID, containerID, sink string) []string {
	args := []string{}

	switch mode {
	case chaostypes.PodModeInject:
		args = []string{
			"network-limitation",
			"inject",
			"--uid",
			string(uid),
			"--metrics-sink",
			sink,
			"--container-id",
			containerID,
			"--bytes-per-sec",
			strconv.Itoa(int(s.BytesPerSec)),
		}
	case chaostypes.PodModeClean:
		args = []string{
			"network-limitation",
			"clean",
			"--uid",
			string(uid),
			"--metrics-sink",
			sink,
			"--container-id",
			containerID,
		}
	}

	return args
}
