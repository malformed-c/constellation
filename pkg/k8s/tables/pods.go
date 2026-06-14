// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package tables

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/cilium/hive/cell"
	"github.com/cilium/statedb"
	"k8s.io/apimachinery/pkg/util/duration"

	slim_corev1 "github.com/cilium/cilium/pkg/k8s/slim/k8s/api/core/v1"
	"github.com/cilium/cilium/pkg/time"
)

// LocalPod is Cilium's internal model of the pods running on this node.
type LocalPod struct {
	*slim_corev1.Pod

	// UpdatedAt is the time when [LocalPod] was last updated, e.g. it
	// shows when the pod change was received from the api-server.
	UpdatedAt time.Time `json:"updatedAt" yaml:"updatedAt"`
}

func (p LocalPod) TableHeader() []string {
	return []string{
		"Name",
		"UID",
		"HostNetwork",
		"PodIPs",
		"Containers",
		"Phase",
		"Age",
	}
}

func (p LocalPod) TableRow() []string {
	podIPs := make([]string, len(p.Status.PodIPs))
	for i := range p.Status.PodIPs {
		podIPs[i] = p.Status.PodIPs[i].IP
	}
	containers := make([]string, len(p.Spec.Containers))
	for i, cont := range p.Spec.Containers {
		ports := make([]string, len(cont.Ports))
		for i, port := range cont.Ports {
			if port.Name != "" {
				ports[i] = fmt.Sprintf("%d/%s (%s)", port.ContainerPort, string(port.Protocol), port.Name)
			} else {
				ports[i] = fmt.Sprintf("%d/%s", port.ContainerPort, string(port.Protocol))
			}
		}
		contName := cont.Name
		if len(ports) > 0 {
			contName += " (" + strings.Join(ports, ",") + ")"
		}
		containers[i] = contName
	}
	return []string{
		p.Namespace + "/" + p.Name,
		string(p.UID),
		strconv.FormatBool(p.Spec.HostNetwork),
		strings.Join(podIPs, ", "),
		strings.Join(containers, ", "),
		string(p.Status.Phase),
		duration.HumanDuration(time.Since(p.UpdatedAt)),
	}
}

var (
	PodNameIndex = newNameIndex[LocalPod]()
	PodTableCell = cell.Provide(NewPodTableAndReflector)
)

func PodByName(namespace, name string) statedb.Query[LocalPod] {
	return PodNameIndex.Query(namespace + "/" + name)
}

func NewPodTable(db *statedb.DB) (statedb.RWTable[LocalPod], error) {
	return statedb.NewTable(
		db,
		"k8s-pods",
		PodNameIndex,
	)
}
