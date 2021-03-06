// Copyright (c) 2016 Intel Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"fmt"
	"regexp"
	"sort"

	"github.com/ciao-project/ciao/ciao-controller/api"
	"github.com/ciao-project/ciao/ciao-controller/types"
	"github.com/gorilla/mux"
)

func instanceToServer(ctl *controller, instance *types.Instance) (api.ServerDetails, error) {
	var volumes []string

	attachments := ctl.ds.GetStorageAttachments(instance.ID)

	for _, vol := range attachments {
		volumes = append(volumes, vol.BlockID)
	}

	server := api.ServerDetails{
		NodeID:     instance.NodeID,
		ID:         instance.ID,
		TenantID:   instance.TenantID,
		WorkloadID: instance.WorkloadID,
		Status:     instance.State,
		PrivateAddresses: []api.PrivateAddresses{
			{
				Addr:    instance.IPAddress,
				MacAddr: instance.MACAddress,
			},
		},
		Volumes: volumes,
		SSHIP:   instance.SSHIP,
		SSHPort: instance.SSHPort,
		Created: instance.CreateTime,
		Name:    instance.Name,
	}

	return server, nil
}

func (c *controller) CreateServer(tenant string, server api.CreateServerRequest) (resp interface{}, err error) {
	nInstances := 1

	if server.Server.MaxInstances > 0 {
		nInstances = server.Server.MaxInstances
	} else if server.Server.MinInstances > 0 {
		nInstances = server.Server.MinInstances
	}

	if server.Server.Name != "" {
		// Between 1 and 64 (HOST_NAME_MAX) alphanum (+ "-")
		r := regexp.MustCompile("^[a-z0-9-]{1,64}$")
		if !r.MatchString(server.Server.Name) {
			return server, types.ErrBadName
		}
	}

	label := server.Server.Metadata["label"]

	w := types.WorkloadRequest{
		WorkloadID: server.Server.WorkloadID,
		TenantID:   tenant,
		Instances:  nInstances,
		TraceLabel: label,
		Name:       server.Server.Name,
	}
	var e error
	instances, err := c.startWorkload(w)
	if err != nil {
		e = err
	}

	var servers api.Servers

	for _, instance := range instances {
		server, err := instanceToServer(c, instance)
		if err != nil && e == nil {
			e = err
		}
		servers.Servers = append(servers.Servers, server)
	}

	if e != nil {
		_ = c.ds.LogError(tenant, fmt.Sprintf("Error launching instance(s): %v", e))
	}

	// If no instances launcher or if none converted bail early
	if e != nil && len(servers.Servers) == 0 {
		return server, e
	}

	servers.TotalServers = len(instances)

	// set machine ID for OpenStack compatibility
	server.Server.ID = instances[0].ID

	// builtServers is define to meet OpenStack compatibility on result
	// format and keep CIAOs legacy behavior.
	builtServers := struct {
		api.CreateServerRequest
		api.Servers
	}{
		api.CreateServerRequest{
			Server: server.Server,
		},
		api.Servers{
			TotalServers: servers.TotalServers,
			Servers:      servers.Servers,
		},
	}

	return builtServers, nil
}

func (c *controller) ListServersDetail(tenant string) ([]api.ServerDetails, error) {
	var servers []api.ServerDetails
	var err error
	var instances []*types.Instance

	if tenant != "" {
		instances, err = c.ds.GetAllInstancesFromTenant(tenant)
	} else {
		instances, err = c.ds.GetAllInstances()
	}

	if err != nil {
		return servers, err
	}

	sort.Sort(types.SortedInstancesByID(instances))

	for _, instance := range instances {
		server, err := instanceToServer(c, instance)
		if err != nil {
			continue
		}

		servers = append(servers, server)
	}

	return servers, nil
}

func (c *controller) ShowServerDetails(tenant string, server string) (api.Server, error) {
	var s api.Server

	instance, err := c.ds.GetTenantInstance(tenant, server)
	if err != nil {
		return s, err
	}

	s.Server, err = instanceToServer(c, instance)
	if err != nil {
		return s, err
	}

	return s, nil
}

func (c *controller) DeleteServer(tenant string, server string) error {
	/* First check that the instance belongs to this tenant */
	_, err := c.ds.GetTenantInstance(tenant, server)
	if err != nil {
		return api.ErrInstanceNotFound
	}

	err = c.deleteInstance(server)

	return err
}

func (c *controller) StartServer(tenant string, ID string) error {
	_, err := c.ds.GetTenantInstance(tenant, ID)
	if err != nil {
		return err
	}

	err = c.restartInstance(ID)

	return err
}

func (c *controller) StopServer(tenant string, ID string) error {
	_, err := c.ds.GetTenantInstance(tenant, ID)
	if err != nil {
		return err
	}

	err = c.stopInstance(ID)

	return err
}

func (c *controller) createComputeRoutes(r *mux.Router) error {
	legacyComputeRoutes(c, r)

	return nil
}
