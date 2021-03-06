/*
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
*/

// Package datastore retrieves stores data for the ciao controller.
// This package caches most data in memory, and uses a sql
// database as persistent storage.
package datastore

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/ciao-project/ciao/ciao-controller/api"
	"github.com/ciao-project/ciao/ciao-controller/types"
	"github.com/ciao-project/ciao/payloads"
	"github.com/ciao-project/ciao/ssntp"
	"github.com/ciao-project/ciao/uuid"
	jsonpatch "github.com/evanphx/json-patch"
	"github.com/golang/glog"
	"github.com/pkg/errors"
)

// custom errors
var (
	ErrNoTenant            = errors.New("Tenant not found")
	ErrNoBlockData         = errors.New("Block Device not found")
	ErrNoStorageAttachment = errors.New("No Volume Attached")
)

// Config contains configuration information for the datastore.
type Config struct {
	DBBackend         persistentStore
	PersistentURI     string
	InitWorkloadsPath string
}

type userEventType string

const (
	userInfo  userEventType = "info"
	userError userEventType = "error"
)

type tenant struct {
	types.Tenant
	network   map[uint32]map[uint32]bool
	instances map[string]*types.Instance
	devices   map[string]types.Volume
	workloads []string
	images    []string
}

type node struct {
	types.Node
	instances map[string]*types.Instance
}

type attachment struct {
	instanceID string
	volumeID   string
}

type tenantIP struct {
	subnet uint32
	host   uint32
}

type persistentStore interface {
	init(config Config) error
	disconnect()

	// interfaces related to logging
	logEvent(event types.LogEntry) error
	clearLog() error
	getEventLog() (logEntries []*types.LogEntry, err error)

	// interfaces related to workloads
	addWorkload(wl types.Workload) error
	deleteWorkload(ID string) error
	getWorkloads() ([]types.Workload, error)

	// interfaces related to tenants
	addTenant(id string, config types.TenantConfig) (err error)
	getTenant(id string) (t *tenant, err error)
	getTenants() ([]*tenant, error)
	releaseTenantIP(tenantID string, subnetInt uint32, rest uint32) (err error)
	claimTenantIP(tenantID string, subnetInt uint32, rest uint32) (err error)
	claimTenantIPs(tenantID string, IPs []tenantIP) (err error)
	updateTenant(tenant *types.Tenant) error
	deleteTenant(tenantID string) error

	// interfaces related to instances
	getInstances() (instances []*types.Instance, err error)
	addInstance(instance *types.Instance) (err error)
	deleteInstance(instanceID string) (err error)
	updateInstance(instance *types.Instance) (err error)

	// interfaces related to statistics
	addNodeStat(stat payloads.Stat) (err error)
	addInstanceStats(stats []payloads.InstanceStat, nodeID string) (err error)
	addFrameStat(stat payloads.FrameTrace) (err error)
	getBatchFrameSummary() (stats []types.BatchFrameSummary, err error)
	getBatchFrameStatistics(label string) (stats []types.BatchFrameStat, err error)

	// storage interfaces
	getWorkloadStorage(ID string) ([]types.StorageResource, error)
	getAllBlockData() (map[string]types.Volume, error)
	addBlockData(data types.Volume) error
	updateBlockData(data types.Volume) error
	deleteBlockData(string) error
	getTenantDevices(tenantID string) (map[string]types.Volume, error)
	addStorageAttachment(a types.StorageAttachment) error
	getAllStorageAttachments() (map[string]types.StorageAttachment, error)
	deleteStorageAttachment(ID string) error

	// external IP interfaces
	addPool(pool types.Pool) error
	updatePool(pool types.Pool) error
	getAllPools() map[string]types.Pool
	deletePool(ID string) error

	addMappedIP(m types.MappedIP) error
	deleteMappedIP(ID string) error
	getMappedIPs() map[string]types.MappedIP

	// quotas
	updateQuotas(tenantID string, qds []types.QuotaDetails) error
	getQuotas(tenantID string) ([]types.QuotaDetails, error)

	// images
	updateImage(i types.Image) error
	deleteImage(ID string) error
	getImages() ([]types.Image, error)
}

// Datastore provides context for the datastore package.
type Datastore struct {
	db persistentStore

	nodeLastStat     map[string]types.CiaoNode
	nodeLastStatLock *sync.RWMutex

	instanceLastStat     map[string]types.CiaoServerStats
	instanceLastStatLock *sync.RWMutex

	tenants     map[string]*tenant
	tenantsLock *sync.RWMutex

	cnciWorkload types.Workload

	nodes     map[string]*node
	nodesLock *sync.RWMutex

	instances     map[string]*types.Instance
	instancesLock *sync.RWMutex

	tenantUsage     map[string][]types.CiaoUsage
	tenantUsageLock *sync.RWMutex

	blockDevices map[string]types.Volume
	bdLock       *sync.RWMutex

	attachments     map[string]types.StorageAttachment
	instanceVolumes map[attachment]string
	attachLock      *sync.RWMutex
	// maybe add a map[instanceid][]types.StorageAttachment
	// to make retrieval of volumes faster.

	pools           map[string]types.Pool
	externalSubnets map[string]bool
	externalIPs     map[string]bool
	mappedIPs       map[string]types.MappedIP
	poolsLock       *sync.RWMutex

	imageLock      *sync.RWMutex
	images         map[string]types.Image
	publicImages   []string
	internalImages []string

	workloadsLock   *sync.RWMutex
	workloads       map[string]types.Workload
	publicWorkloads []string
}

func (ds *Datastore) initExternalIPs() {
	ds.poolsLock = &sync.RWMutex{}
	ds.externalSubnets = make(map[string]bool)
	ds.externalIPs = make(map[string]bool)

	ds.pools = ds.db.getAllPools()

	for _, pool := range ds.pools {
		for _, subnet := range pool.Subnets {
			ds.externalSubnets[subnet.CIDR] = true
		}

		for _, IP := range pool.IPs {
			ds.externalIPs[IP.Address] = true
		}
	}

	ds.mappedIPs = ds.db.getMappedIPs()
}

func (ds *Datastore) initImages() error {
	ds.imageLock = &sync.RWMutex{}
	ds.images = make(map[string]types.Image)
	images, err := ds.db.getImages()
	if err != nil {
		return errors.Wrap(err, "error getting images from database")
	}
	for _, i := range images {
		ds.images[i.ID] = i

		if i.Visibility == types.Public {
			ds.publicImages = append(ds.publicImages, i.ID)
		}

		if i.Visibility == types.Internal {
			ds.internalImages = append(ds.internalImages, i.ID)
		}

		if i.TenantID != "" {
			_, ok := ds.tenants[i.TenantID]
			if !ok {
				return errors.Wrapf(err, "Database inconsistent: tenant in images not in database: %s", i.TenantID)
			}

			ds.tenants[i.TenantID].images = append(ds.tenants[i.TenantID].images, i.ID)
		}
	}
	return nil
}

func (ds *Datastore) initWorkloads() error {
	ds.workloadsLock = &sync.RWMutex{}
	ds.workloads = make(map[string]types.Workload)
	workloads, err := ds.db.getWorkloads()
	if err != nil {
		return errors.Wrap(err, "error getting workloads from database")
	}

	for _, wl := range workloads {
		ds.workloads[wl.ID] = wl

		if wl.Visibility == types.Public {
			ds.publicWorkloads = append(ds.publicWorkloads, wl.ID)
		}

		if wl.TenantID != "" {
			_, ok := ds.tenants[wl.TenantID]
			if !ok {
				return errors.Wrapf(err, "Database inconsistent: tenant in workload not in database: %s", wl.TenantID)
			}

			ds.tenants[wl.TenantID].workloads = append(ds.tenants[wl.TenantID].workloads, wl.ID)
		}
	}

	return nil
}

// Init initializes the private data for the Datastore object.
// The sql tables are populated with initial data from csv
// files if this is the first time the database has been
// created.  The datastore caches are also filled.
func (ds *Datastore) Init(config Config) error {
	ps := config.DBBackend

	if ps == nil {
		ps = &sqliteDB{}
	}

	err := ps.init(config)
	if err != nil {
		return errors.Wrap(err, "error initialising persistent store")
	}

	ds.db = ps

	ds.nodeLastStat = make(map[string]types.CiaoNode)
	ds.nodeLastStatLock = &sync.RWMutex{}

	ds.instanceLastStat = make(map[string]types.CiaoServerStats)
	ds.instanceLastStatLock = &sync.RWMutex{}

	// warning, do not use the tenant cache to get
	// networking information right now.  that is not
	// updated, just the resources
	ds.tenants = make(map[string]*tenant)
	ds.tenantsLock = &sync.RWMutex{}

	// cache all our instances prior to getting tenants
	ds.instancesLock = &sync.RWMutex{}
	ds.instances = make(map[string]*types.Instance)

	instances, err := ds.db.getInstances()
	if err != nil {
		return errors.Wrap(err, "error getting instances from database")
	}

	for i := range instances {
		ds.instances[instances[i].ID] = instances[i]
	}

	// cache our current tenants into a map that we can
	// quickly index
	tenants, err := ds.db.getTenants()
	if err != nil {
		return errors.Wrap(err, "error getting tenants from database")
	}
	for i := range tenants {
		ds.tenants[tenants[i].ID] = tenants[i]
	}

	err = ds.initImages()
	if err != nil {
		return errors.Wrap(err, "error initialising images")
	}

	err = ds.initWorkloads()
	if err != nil {
		return errors.Wrap(err, "error initialising workloads")
	}

	ds.nodesLock = &sync.RWMutex{}
	ds.nodes = make(map[string]*node)

	for key, i := range ds.instances {
		_, ok := ds.nodes[i.NodeID]
		if !ok {
			newNode := types.Node{
				ID: i.NodeID,
			}
			n := &node{
				Node:      newNode,
				instances: make(map[string]*types.Instance),
			}
			ds.nodes[i.NodeID] = n
		}
		ds.nodes[i.NodeID].instances[key] = i

		// ds.tenants.instances should point to the same
		// instances that we have in ds.instances, otherwise they
		// will not get updated when we get new stats.

		tenant := ds.tenants[i.TenantID]
		if tenant != nil {
			tenant.instances[i.ID] = i
		}
	}

	ds.tenantUsage = make(map[string][]types.CiaoUsage)
	ds.tenantUsageLock = &sync.RWMutex{}

	ds.blockDevices, err = ds.db.getAllBlockData()
	if err != nil {
		return errors.Wrap(err, "error getting block devices from database")
	}

	ds.bdLock = &sync.RWMutex{}

	ds.attachments, err = ds.db.getAllStorageAttachments()
	if err != nil {
		return errors.Wrap(err, "error getting storage attachments from database")
	}

	ds.instanceVolumes = make(map[attachment]string)

	for key, value := range ds.attachments {
		link := attachment{
			instanceID: value.InstanceID,
			volumeID:   value.BlockID,
		}

		ds.instanceVolumes[link] = key
	}

	ds.attachLock = &sync.RWMutex{}

	ds.initExternalIPs()

	return nil
}

// Exit will disconnect the backing database.
func (ds *Datastore) Exit() {
	ds.db.disconnect()
}

// AddTenant stores information about a tenant into the datastore.
// and makes sure that this new tenant is cached.
func (ds *Datastore) AddTenant(id string, config types.TenantConfig) (*types.Tenant, error) {
	ds.tenantsLock.Lock()
	defer ds.tenantsLock.Unlock()

	t, ok := ds.tenants[id]
	if ok {
		return nil, errors.New("Duplicate Tenant ID")
	}

	err := ds.db.addTenant(id, config)
	if err != nil {
		return nil, errors.Wrapf(err, "error adding tenant (%v) to database", id)
	}

	t, err = ds.db.getTenant(id)
	if err != nil || t == nil {
		return nil, err
	}

	ds.tenants[id] = t

	return &t.Tenant, nil
}

// DeleteTenant removes a tenant from the datastore.
// It is the responsibility of the caller to ensure all tenant artifacts
// are removed first.
func (ds *Datastore) DeleteTenant(ID string) error {
	ds.tenantsLock.Lock()
	defer ds.tenantsLock.Unlock()

	_, ok := ds.tenants[ID]
	if !ok {
		return ErrNoTenant
	}

	delete(ds.tenants, ID)

	return ds.db.deleteTenant(ID)
}

func (ds *Datastore) getTenant(id string) (*tenant, error) {
	// check cache first
	ds.tenantsLock.RLock()
	t := ds.tenants[id]
	ds.tenantsLock.RUnlock()

	if t != nil {
		return t, nil
	}

	t, err := ds.db.getTenant(id)
	return t, errors.Wrapf(err, "error getting tenant (%v) from database", id)
}

// GetTenant returns details about a tenant referenced by the uuid
func (ds *Datastore) GetTenant(id string) (*types.Tenant, error) {
	t, err := ds.getTenant(id)
	if err != nil || t == nil {
		return nil, err
	}

	return &t.Tenant, nil
}

// JSONPatchTenant will update a tenant with changes from a json merge patch.
func (ds *Datastore) JSONPatchTenant(ID string, patch []byte) error {
	var config types.TenantConfig

	ds.tenantsLock.Lock()
	defer ds.tenantsLock.Unlock()

	tenant, ok := ds.tenants[ID]
	if !ok {
		return ErrNoTenant
	}

	oldconfig := tenant.TenantConfig

	orig, err := json.Marshal(oldconfig)
	if err != nil {
		return errors.Wrap(err, "error updating tenant")
	}

	new, err := jsonpatch.MergePatch(orig, patch)
	if err != nil {
		return errors.Wrap(err, "error updating tenant")
	}

	err = json.Unmarshal(new, &config)
	if err != nil {
		return errors.Wrap(err, "error updating tenant")
	}

	// SubnetBits must not modified if there are active instances.
	// for now, the cncis must also be removed. In the future we might
	// be able to just update the cnci with the new subnet info.
	if len(tenant.instances) > 0 {
		if oldconfig.SubnetBits != config.SubnetBits {
			return errors.New("Unable to update with active instances")
		}
	}

	tenant.TenantConfig = config

	return ds.db.updateTenant(&tenant.Tenant)
}

// AddWorkload is used to add a new workload to the datastore.
// Both cache and persistent store are updated.
func (ds *Datastore) AddWorkload(w types.Workload) error {
	ds.workloadsLock.Lock()
	defer ds.workloadsLock.Unlock()

	err := ds.db.addWorkload(w)
	if err != nil {
		return errors.Wrapf(err, "error updating workload (%v) in database", w.ID)
	}

	ds.workloads[w.ID] = w
	if w.Visibility == types.Public {
		ds.publicWorkloads = append(ds.publicWorkloads, w.ID)
	} else {
		ds.tenantsLock.Lock()
		defer ds.tenantsLock.Unlock()
		tenant, ok := ds.tenants[w.TenantID]
		if !ok {
			return ErrNoTenant
		}

		tenant.workloads = append(tenant.workloads, w.ID)
	}
	return nil
}

// DeleteWorkload will delete an unused workload from the datastore.
// workload ID out of the datastore.
func (ds *Datastore) DeleteWorkload(workloadID string) error {
	ds.workloadsLock.Lock()
	defer ds.workloadsLock.Unlock()

	// make sure that this workload is not in use.
	// always get from cache
	ds.instancesLock.RLock()
	defer ds.instancesLock.RUnlock()

	for _, val := range ds.instances {
		if val.WorkloadID == workloadID {
			// we can't go on.
			return types.ErrWorkloadInUse
		}
	}

	wl, ok := ds.workloads[workloadID]
	if !ok {
		return types.ErrWorkloadNotFound
	}

	err := ds.db.deleteWorkload(workloadID)
	if err != nil {
		return errors.Wrapf(err, "error deleting workload %v from database", workloadID)
	}

	if wl.Visibility == types.Public {
		for i, id := range ds.publicWorkloads {
			if id == workloadID {
				ds.publicWorkloads = append(ds.publicWorkloads[:i], ds.publicWorkloads[i+1:]...)
				break
			}
		}
	} else {
		ds.tenantsLock.Lock()
		defer ds.tenantsLock.Unlock()

		t, ok := ds.tenants[wl.TenantID]
		if !ok {
			return types.ErrTenantNotFound
		}

		for i, id := range t.workloads {
			if id == workloadID {
				ds.tenants[wl.TenantID].workloads = append(ds.tenants[wl.TenantID].workloads[:i], ds.tenants[wl.TenantID].workloads[i+1:]...)
				break
			}
		}
	}

	delete(ds.workloads, workloadID)

	return nil
}

// GetWorkload returns details about a specific workload referenced by id
func (ds *Datastore) GetWorkload(ID string) (types.Workload, error) {
	if ID == ds.cnciWorkload.ID {
		return ds.cnciWorkload, nil
	}

	ds.workloadsLock.RLock()
	defer ds.workloadsLock.RUnlock()

	workload, ok := ds.workloads[ID]
	if ok {
		return workload, nil
	}

	return types.Workload{}, types.ErrWorkloadNotFound
}

// GetWorkloads retrieves the list of workloads for a particular tenant.
// if there are any public workloads, they will be included in the returned list.
func (ds *Datastore) GetWorkloads(tenantID string) ([]types.Workload, error) {
	return ds.getWorkloads(tenantID, true)
}

// GetTenantWorkloads retrieves a list of private workloads.
func (ds *Datastore) GetTenantWorkloads(tenantID string) ([]types.Workload, error) {
	return ds.getWorkloads(tenantID, false)
}

func (ds *Datastore) getWorkloads(tenantID string, includePublic bool) ([]types.Workload, error) {
	var workloads []types.Workload

	ds.workloadsLock.RLock()
	defer ds.workloadsLock.RUnlock()

	ds.tenantsLock.RLock()
	defer ds.tenantsLock.RUnlock()

	if includePublic {
		for _, id := range ds.publicWorkloads {
			workloads = append(workloads, ds.workloads[id])
		}
	}

	// if there isn't a tenant here, it isn't necessarily an
	// error.
	tenant, ok := ds.tenants[tenantID]
	if !ok {
		return workloads, nil
	}

	for _, id := range tenant.workloads {
		workloads = append(workloads, ds.workloads[id])
	}

	return workloads, nil
}

// UpdateInstance will update certain fields of an instance
func (ds *Datastore) UpdateInstance(instance *types.Instance) error {
	return ds.db.updateInstance(instance)
}

// GetAllTenants returns all the tenants from the datastore.
func (ds *Datastore) GetAllTenants() ([]*types.Tenant, error) {
	var tenants []*types.Tenant

	for _, t := range ds.tenants {
		tenants = append(tenants, &t.Tenant)
	}

	return tenants, nil
}

// ReleaseTenantIP will return an IP address previously allocated to the pool.
// Once a tenant IP address is released, it can be reassigned to another
// instance.
func (ds *Datastore) ReleaseTenantIP(tenantID string, ip string) error {
	removeSubnet := false
	var i uint32

	ipAddr := net.ParseIP(ip)
	if ipAddr == nil {
		return errors.New("Invalid IPv4 Address")
	}

	tenant, err := ds.GetTenant(tenantID)
	if err != nil {
		return err
	}

	mask := net.CIDRMask(tenant.SubnetBits, 32)
	ipNet := net.IPNet{
		IP:   ipAddr.Mask(mask),
		Mask: mask,
	}
	subMask := binary.BigEndian.Uint32(ipNet.Mask)
	hostInt := binary.BigEndian.Uint32(ipAddr.To4())
	subnetInt := hostInt & subMask

	// clear from cache
	ds.tenantsLock.Lock()

	if ds.tenants[tenantID] != nil {
		delete(ds.tenants[tenantID].network[subnetInt], hostInt)
		network := ds.tenants[tenantID].network
		i = subnetInt

		if len(network[i]) == 0 {
			// delete the network map and the subnet
			delete(ds.tenants[tenantID].network, i)

			removeSubnet = true
		}
	}

	if removeSubnet && ds.tenants[tenantID].CNCIctrl != nil {
		err := ds.tenants[tenantID].CNCIctrl.ScheduleRemoveSubnet(ipNet.String())
		if err != nil {
			glog.Warningf("Unable to remove subnet (%v)", err)
		}
	}

	ds.tenantsLock.Unlock()

	return ds.db.releaseTenantIP(tenantID, subnetInt, hostInt)
}

// lock for tenant must be held.
func (ds *Datastore) cleanTenantIPs(tenantID string, IPs []tenantIP) {
	for _, IP := range IPs {
		delete(ds.tenants[tenantID].network[IP.subnet], IP.host)

		network := ds.tenants[tenantID].network
		if len(network[IP.subnet]) == 0 {
			delete(ds.tenants[tenantID].network, IP.subnet)
		}
	}
}

// lock for tenant must not be held here.
func (ds *Datastore) activateSubnets(tenantID string, IPs []net.IP) error {
	mgr := ds.tenants[tenantID].CNCIctrl
	if mgr == nil {
		return nil
	}

	tenant := ds.tenants[tenantID]
	mask := net.CIDRMask(tenant.SubnetBits, 32)

	for _, ip := range IPs {
		ipnet := net.IPNet{
			IP:   ip.Mask(mask),
			Mask: mask,
		}

		err := mgr.WaitForActive(ipnet.String())
		if err != nil {
			return err
		}
	}
	return nil
}

// AllocateTenantIPPool will reserve a pool of IP addresses for the caller.
func (ds *Datastore) AllocateTenantIPPool(tenantID string, num int) ([]net.IP, error) {
	var addrs []net.IP
	var tenantAddrs []tenantIP
	var retval error
	tenant, err := ds.GetTenant(tenantID)
	if err != nil {
		return nil, err
	}

	// hardcode start address and max address for tenant network.
	cidr := fmt.Sprintf("%s/%d", "172.16.0.0", tenant.SubnetBits)
	IP, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, err
	}

	start := binary.BigEndian.Uint32(IP.Mask(ipNet.Mask))
	end := start >> 20
	end = end + uint32(1)
	end = end << 20
	ones, bits := ipNet.Mask.Size()
	hostBits := uint32(bits - ones)
	maxHosts := (1 << hostBits)
	mask := binary.BigEndian.Uint32(ipNet.Mask)

	var hostCount int

	ds.tenantsLock.Lock()
	defer func() {
		ds.tenantsLock.Unlock()

		// try to start any subnets that need it.
		// This will modify retval if there was an
		// error.
		if addrs != nil {
			retval = ds.activateSubnets(tenantID, addrs)
		}
	}()

	subnets := ds.tenants[tenantID].network

	// look for any subnets that have available host nums
	for k, v := range subnets {
		if len(v) < maxHosts {
			start = k
			break
		}
	}

	for {
		if start >= end {
			ds.cleanTenantIPs(tenantID, tenantAddrs)
			addrs = nil
			return nil, errors.New("out of addrs")
		}

		// if we have not yet allocated out of this subnet,
		// we need to make a new map to hold the host addrs.
		subnetNum := start & mask
		if subnets[subnetNum] == nil {
			subnets[subnetNum] = make(map[uint32]bool)
		}
		netmap := subnets[subnetNum]

		// skip network, gateway, and broadcast addrs.
		for host := 2; host < maxHosts-1; host++ {
			if netmap[start+uint32(host)] == false {
				addr := start + uint32(host)
				netmap[addr] = true
				newIP := make(net.IP, net.IPv4len)
				binary.BigEndian.PutUint32(newIP, addr)
				addrs = append(addrs, newIP)
				tenantAddrs = append(tenantAddrs, tenantIP{subnetNum, addr})
				hostCount++
				if hostCount == num {
					// attempt bulk db insert here.
					err := ds.db.claimTenantIPs(tenantID, tenantAddrs)
					if err != nil {
						ds.cleanTenantIPs(tenantID, tenantAddrs)
						addrs = nil
						return nil, err
					}

					// go ahead and return the IPs to the
					// user but possibly with error.
					return addrs, retval
				}
			}
		}

		// skip to the start of the next subnet
		start += uint32(maxHosts)
	}
}

// AllocateTenantIP will allocate a single IP address for a tenant.
func (ds *Datastore) AllocateTenantIP(tenantID string) (net.IP, error) {
	ips, err := ds.AllocateTenantIPPool(tenantID, 1)
	if err != nil {
		return nil, err
	}
	return ips[0], nil
}

func (ds *Datastore) getInstances(cncis bool) ([]*types.Instance, error) {
	var instances []*types.Instance

	// always get from cache
	ds.instancesLock.RLock()

	if len(ds.instances) > 0 {
		for _, val := range ds.instances {
			if val.CNCI == cncis {
				instances = append(instances, val)
			}
		}
	}

	ds.instancesLock.RUnlock()

	return instances, nil
}

// GetAllInstances retrieves all tenant instances out of the datastore.
func (ds *Datastore) GetAllInstances() ([]*types.Instance, error) {
	return ds.getInstances(false)
}

// GetAllCNCIInstances retrieves all CNCI instances out of the datastore.
func (ds *Datastore) GetAllCNCIInstances() ([]*types.Instance, error) {
	return ds.getInstances(true)
}

// GetInstance retrieves an instance out of the datastore.
// The CNCI could be retrieved this way.
func (ds *Datastore) GetInstance(id string) (*types.Instance, error) {
	// always get from cache
	ds.instancesLock.RLock()

	value, ok := ds.instances[id]

	ds.instancesLock.RUnlock()

	if !ok {
		return nil, types.ErrInstanceNotFound
	}

	return value, nil
}

// GetTenantInstance retrieves a tenant instance out of the datastore.
// the CNCI will be excluded from this search.
func (ds *Datastore) GetTenantInstance(tenantID string, instanceID string) (*types.Instance, error) {
	// always get from cache
	ds.instancesLock.RLock()

	value, ok := ds.instances[instanceID]

	ds.instancesLock.RUnlock()

	if !ok || value.TenantID != tenantID || value.CNCI == true {
		return nil, types.ErrInstanceNotFound
	}

	return value, nil
}

func (ds *Datastore) getTenantInstances(tenantID string, cncis bool) ([]*types.Instance, error) {
	var instances []*types.Instance

	ds.tenantsLock.RLock()

	t, ok := ds.tenants[tenantID]
	if ok {
		for _, val := range t.instances {
			if val.CNCI == cncis {
				instances = append(instances, val)
			}
		}

		ds.tenantsLock.RUnlock()

		return instances, nil
	}

	ds.tenantsLock.RUnlock()

	return nil, nil
}

// GetAllInstancesFromTenant will retrieve all instances belonging to a specific tenant.
// This will exclude any CNCI instances.
func (ds *Datastore) GetAllInstancesFromTenant(tenantID string) ([]*types.Instance, error) {
	return ds.getTenantInstances(tenantID, false)
}

// GetTenantCNCIs will retrieve all CNCI instances belonging to a tenant
func (ds *Datastore) GetTenantCNCIs(tenantID string) ([]*types.Instance, error) {
	return ds.getTenantInstances(tenantID, true)
}

// GetAllInstancesByNode will retrieve all the instances running on a specific compute Node.
func (ds *Datastore) GetAllInstancesByNode(nodeID string) ([]*types.Instance, error) {
	var instances []*types.Instance

	ds.nodesLock.RLock()

	n, ok := ds.nodes[nodeID]
	if ok {
		for _, val := range n.instances {
			if val.CNCI == false {
				instances = append(instances, val)
			}
		}
	}

	ds.nodesLock.RUnlock()

	return instances, nil
}

// AddInstance will store a new instance in the datastore.
// The instance will be updated both in the cache and in the database
func (ds *Datastore) AddInstance(instance *types.Instance) error {
	err := ds.db.addInstance(instance)

	if err != nil {
		return errors.Wrap(err, "Error adding instance to database")
	}

	// add to cache
	ds.instancesLock.Lock()

	ds.instances[instance.ID] = instance

	instanceStat := types.CiaoServerStats{
		ID:        instance.ID,
		TenantID:  instance.TenantID,
		NodeID:    instance.NodeID,
		Timestamp: time.Now(),
		Status:    instance.State,
	}

	ds.instanceLastStatLock.Lock()
	ds.instanceLastStat[instance.ID] = instanceStat
	ds.instanceLastStatLock.Unlock()

	ds.instancesLock.Unlock()

	ds.tenantsLock.Lock()
	tenant := ds.tenants[instance.TenantID]
	if tenant != nil {
		tenant.instances[instance.ID] = instance
	}
	ds.tenantsLock.Unlock()

	return nil
}

// StartFailure will clean up after a failure to start an instance.
// If an instance was a CNCI, this function will remove the CNCI instance
// for this tenant. If the instance was a normal tenant instance, the
// IP address will be released and the instance will be deleted from the
// datastore.
//
// Only instances whose status is pending are removed when a StartFailure event
// is received.  StartFailure errors may also be generated when restarting an
// exited instance and we want to make sure that a failure to restart such
// an instance does not result in it being deleted.
func (ds *Datastore) StartFailure(instanceID string, reason payloads.StartFailureReason, migration bool, nodeID string) error {
	i, err := ds.GetInstance(instanceID)
	if err != nil {
		return errors.Wrapf(err, "error getting instance (%v)", instanceID)
	}

	if i.CNCI == true {
		glog.Warning("CNCI ", instanceID, " Failed to start")
	}

	if reason.IsFatal() && !migration {
		if _, err := ds.deleteInstance(instanceID); err != nil {
			return errors.Wrap(err, "Error deleting instance")
		}
	}

	ds.nodesLock.Lock()
	defer ds.nodesLock.Unlock()

	n, ok := ds.nodes[nodeID]
	if ok {
		n.TotalFailures++
		n.StartFailures++
	}

	msg := fmt.Sprintf("Start Failure %s: %s", instanceID, reason.String())
	e := types.LogEntry{
		TenantID:  i.TenantID,
		EventType: string(userError),
		Message:   msg,
		NodeID:    nodeID,
	}
	return errors.Wrap(ds.db.logEvent(e), "Error logging event")
}

// AttachVolumeFailure will clean up after a failure to attach a volume.
// The volume state will be changed back to available, and an error message
// will be logged.
func (ds *Datastore) AttachVolumeFailure(instanceID string, volumeID string, reason payloads.AttachVolumeFailureReason) error {
	// update the block data to reflect correct state
	data, err := ds.GetBlockDevice(volumeID)
	if err != nil {
		return errors.Wrapf(err, "error getting block device for volume (%v)", volumeID)
	}

	oldState := data.State
	data.State = types.Available
	err = ds.UpdateBlockDevice(data)
	if err != nil {
		data.State = oldState
		return errors.Wrapf(err, "error updating block device for volume (%v)", volumeID)
	}

	// get owner of this instance
	i, err := ds.GetInstance(instanceID)
	if err != nil {
		return errors.Wrapf(err, "error getting instance (%v)", instanceID)
	}

	ds.nodesLock.Lock()
	defer ds.nodesLock.Unlock()

	n, ok := ds.nodes[i.NodeID]
	if ok {
		n.TotalFailures++
		n.AttachVolumeFailures++
	}

	msg := fmt.Sprintf("Attach Volume Failure %s to %s: %s", volumeID, instanceID, reason.String())
	e := types.LogEntry{
		TenantID:  i.TenantID,
		EventType: string(userError),
		Message:   msg,
		NodeID:    i.NodeID,
	}

	return errors.Wrap(ds.db.logEvent(e), "Error logging event")
}

func (ds *Datastore) deleteInstance(instanceID string) (string, error) {
	if err := ds.db.deleteInstance(instanceID); err != nil {
		glog.Warningf("error deleting instance (%v): %v", instanceID, err)
		return "", errors.Wrapf(err, "error deleting instance from database (%v)", instanceID)
	}

	ds.instanceLastStatLock.Lock()
	delete(ds.instanceLastStat, instanceID)
	ds.instanceLastStatLock.Unlock()

	ds.instancesLock.Lock()
	i := ds.instances[instanceID]
	delete(ds.instances, instanceID)
	ds.instancesLock.Unlock()

	ds.tenantsLock.Lock()
	tenant := ds.tenants[i.TenantID]
	if tenant != nil {
		delete(tenant.instances, instanceID)
	}
	ds.tenantsLock.Unlock()

	// we may not have received any node stats for this instance
	if i.NodeID != "" {
		ds.nodesLock.Lock()
		delete(ds.nodes[i.NodeID].instances, instanceID)
		ds.nodesLock.Unlock()
	}

	var err error
	if tmpErr := ds.db.deleteInstance(i.ID); tmpErr != nil {
		glog.Warningf("error deleting instance (%v): %v", i.ID, err)
		err = errors.Wrapf(tmpErr, "error deleting instance from database (%v)", i.ID)
	}

	if i.CNCI == false {
		if tmpErr := ds.ReleaseTenantIP(i.TenantID, i.IPAddress); tmpErr != nil {
			glog.Warningf("error releasing IP for instance (%v): %v", i.ID, tmpErr)
			if err == nil {
				err = errors.Wrapf(err, "error releasing IP for instance (%v)", i.ID)
			}
		}
	}

	ds.updateStorageAttachments(instanceID)

	return i.TenantID, err
}

// DeleteInstance removes an instance from the datastore.
func (ds *Datastore) DeleteInstance(instanceID string) error {
	i, err := ds.GetInstance(instanceID)
	if err != nil {
		return errors.Wrapf(err, "error deleting instance")
	}

	nodeID := i.NodeID

	tenantID, err := ds.deleteInstance(instanceID)
	if err != nil {
		return errors.Wrapf(err, "error deleting instance")
	}

	msg := fmt.Sprintf("Deleted Instance %s", instanceID)
	e := types.LogEntry{
		TenantID:  tenantID,
		EventType: string(userInfo),
		Message:   msg,
		NodeID:    nodeID,
	}
	return errors.Wrap(ds.db.logEvent(e), "Error logging event")
}

func (ds *Datastore) updateInstanceStatus(status, instanceID string) error {
	stats := []payloads.InstanceStat{
		{
			InstanceUUID: instanceID,
			State:        status,
		},
	}

	err := ds.db.addInstanceStats(stats, "")
	if err != nil {
		return errors.Wrapf(err, "error adding instance stats to database")
	}

	instanceStat := types.CiaoServerStats{
		ID:        instanceID,
		Timestamp: time.Now(),
		Status:    status,
	}
	ds.instanceLastStatLock.Lock()
	ds.instanceLastStat[instanceID] = instanceStat
	ds.instanceLastStatLock.Unlock()

	return nil
}

// InstanceRestarting resets a restarting instance's state to pending.
func (ds *Datastore) InstanceRestarting(instanceID string) error {
	err := ds.updateInstanceStatus(payloads.Pending, instanceID)
	if err != nil {
		return errors.Wrap(err, "Error marking instance as restarting")
	}

	ds.instancesLock.Lock()
	i := ds.instances[instanceID]
	i.State = payloads.Pending
	ds.instancesLock.Unlock()

	return nil
}

// InstanceStopped removes the link between an instance and its node
func (ds *Datastore) InstanceStopped(instanceID string) error {
	err := ds.updateInstanceStatus(payloads.Exited, instanceID)
	if err != nil {
		return errors.Wrap(err, "Error marked instance as stopped")
	}

	ds.instancesLock.Lock()
	i := ds.instances[instanceID]
	oldNodeID := i.NodeID
	i.NodeID = ""
	i.State = payloads.Exited
	ds.instancesLock.Unlock()

	// we may not have received any node stats for this instance
	if oldNodeID != "" {
		ds.nodesLock.Lock()
		delete(ds.nodes[oldNodeID].instances, instanceID)
		ds.nodesLock.Unlock()
	}

	return nil
}

// DeleteNode removes a node from the node cache.
func (ds *Datastore) DeleteNode(nodeID string) error {
	ds.nodesLock.Lock()
	for _, i := range ds.nodes[nodeID].instances {
		_ = i.TransitionInstanceState(payloads.Missing)
		i.NodeID = ""
	}
	delete(ds.nodes, nodeID)
	ds.nodesLock.Unlock()

	ds.nodeLastStatLock.Lock()
	delete(ds.nodeLastStat, nodeID)
	ds.nodeLastStatLock.Unlock()

	return nil
}

// AddNode adds a node into the node cache, updating the node's tracked
// role bitmask if the node is already present to be the superset of all
// reported roles.
func (ds *Datastore) AddNode(nodeID string, nodeType payloads.Resource) {
	var role ssntp.Role
	switch nodeType {
	case payloads.ComputeNode:
		role = ssntp.AGENT
	case payloads.NetworkNode:
		role = ssntp.NETAGENT
	}

	ds.nodesLock.Lock()
	defer ds.nodesLock.Unlock()

	if ds.nodes[nodeID] != nil {
		ds.nodes[nodeID].NodeRole |= role
		return
	}

	n := &node{
		Node: types.Node{
			ID:       nodeID,
			NodeRole: role,
		},
		instances: make(map[string]*types.Instance),
	}
	ds.nodes[nodeID] = n
}

// GetNode retrieves a node in the node cache.
func (ds *Datastore) GetNode(nodeID string) (types.Node, error) {
	var node types.Node

	ds.nodesLock.RLock()
	defer ds.nodesLock.RUnlock()

	if ds.nodes[nodeID] == nil {
		return node, fmt.Errorf("node %s not found", nodeID)
	}

	return ds.nodes[nodeID].Node, nil
}

// HandleStats makes sure that the data from the stat payload is stored.
func (ds *Datastore) HandleStats(stat payloads.Stat) error {
	if stat.Load != -1 {
		if err := ds.addNodeStat(stat); err != nil {
			return errors.Wrap(err, "error updating node stats")
		}
	}

	return errors.Wrapf(ds.addInstanceStats(stat.Instances, stat.NodeUUID), "error updating stats")
}

// HandleTraceReport stores the provided trace data in the datastore.
func (ds *Datastore) HandleTraceReport(trace payloads.Trace) error {
	var err error
	for index := range trace.Frames {
		i := trace.Frames[index]

		if tmpErr := ds.db.addFrameStat(i); tmpErr != nil {
			if err == nil {
				err = errors.Wrapf(tmpErr, "error adding stats to database")
			}
		}
	}

	return err
}

// GetInstanceLastStats retrieves the last instances stats received for this node.
// It returns it in a format suitable for the compute API.
func (ds *Datastore) GetInstanceLastStats(nodeID string) types.CiaoServersStats {
	var serversStats types.CiaoServersStats

	ds.instanceLastStatLock.RLock()
	for _, instance := range ds.instanceLastStat {
		if instance.NodeID != nodeID {
			continue
		}

		i, err := ds.GetInstance(instance.ID)
		if err != nil {
			glog.Warningf("skipping stat for instance %s: %v", instance.ID, err)
			continue
		}

		if i.CNCI != true {
			serversStats.Servers = append(serversStats.Servers, instance)
		}
	}
	ds.instanceLastStatLock.RUnlock()

	return serversStats
}

// GetNodeLastStats retrieves the last nodes' stats received.
// It returns it in a format suitable for the compute API.
func (ds *Datastore) GetNodeLastStats() types.CiaoNodes {
	var nodes types.CiaoNodes

	ds.nodeLastStatLock.RLock()
	for _, node := range ds.nodeLastStat {
		nodes.Nodes = append(nodes.Nodes, node)
	}
	ds.nodeLastStatLock.RUnlock()

	return nodes
}

func (ds *Datastore) addNodeStat(stat payloads.Stat) error {
	ds.nodesLock.Lock()

	n, ok := ds.nodes[stat.NodeUUID]
	if !ok {
		n = &node{}
		n.instances = make(map[string]*types.Instance)
		ds.nodes[stat.NodeUUID] = n
	}

	n.ID = stat.NodeUUID
	n.Hostname = stat.NodeHostName

	cnStat := types.CiaoNode{
		ID:                   stat.NodeUUID,
		Hostname:             n.Hostname,
		Status:               stat.Status,
		Load:                 stat.Load,
		MemTotal:             stat.MemTotalMB,
		MemAvailable:         stat.MemAvailableMB,
		DiskTotal:            stat.DiskTotalMB,
		DiskAvailable:        stat.DiskAvailableMB,
		OnlineCPUs:           stat.CpusOnline,
		TotalFailures:        n.TotalFailures,
		StartFailures:        n.StartFailures,
		AttachVolumeFailures: n.AttachVolumeFailures,
		DeleteFailures:       n.DeleteFailures,
	}

	ds.nodesLock.Unlock()
	ds.nodeLastStatLock.Lock()

	delete(ds.nodeLastStat, stat.NodeUUID)
	ds.nodeLastStat[stat.NodeUUID] = cnStat

	ds.nodeLastStatLock.Unlock()

	return errors.Wrap(ds.db.addNodeStat(stat), "error adding node stats to database")
}

var tenantUsagePeriodMinutes float64 = 5

func (ds *Datastore) updateTenantUsageNeeded(delta types.CiaoUsage, tenantID string) bool {
	if delta.VCPU == 0 &&
		delta.Memory == 0 &&
		delta.Disk == 0 {
		return false
	}

	return true
}

func (ds *Datastore) updateTenantUsage(delta types.CiaoUsage, tenantID string) {
	if ds.updateTenantUsageNeeded(delta, tenantID) == false {
		return
	}

	createNewUsage := true
	lastUsage := types.CiaoUsage{}

	ds.tenantUsageLock.Lock()

	tenantUsage := ds.tenantUsage[tenantID]
	if len(tenantUsage) != 0 {
		lastUsage = tenantUsage[len(tenantUsage)-1]
		// We will not create more than one entry per tenant every tenantUsagePeriodMinutes
		if time.Since(lastUsage.Timestamp).Minutes() < tenantUsagePeriodMinutes {
			createNewUsage = false
		}
	}

	newUsage := types.CiaoUsage{
		VCPU:   lastUsage.VCPU + delta.VCPU,
		Memory: lastUsage.Memory + delta.Memory,
		Disk:   lastUsage.Disk + delta.Disk,
	}

	// If we need to create a new usage entry, we timestamp it now.
	// If not we just update the last entry.
	if createNewUsage == true {
		newUsage.Timestamp = time.Now()
		ds.tenantUsage[tenantID] = append(ds.tenantUsage[tenantID], newUsage)
	} else {
		newUsage.Timestamp = lastUsage.Timestamp
		tenantUsage[len(tenantUsage)-1] = newUsage
	}

	ds.tenantUsageLock.Unlock()
}

// GetTenantUsage provides statistics on actual resource usage.
// Usage is provided between a specified time period.
func (ds *Datastore) GetTenantUsage(tenantID string, start time.Time, end time.Time) ([]types.CiaoUsage, error) {
	ds.tenantUsageLock.RLock()
	defer ds.tenantUsageLock.RUnlock()

	tenantUsage := ds.tenantUsage[tenantID]
	if tenantUsage == nil || len(tenantUsage) == 0 {
		return nil, nil
	}

	historyLength := len(tenantUsage)
	if tenantUsage[0].Timestamp.After(end) == true ||
		start.After(tenantUsage[historyLength-1].Timestamp) == true {
		return nil, nil
	}

	first := 0
	last := 0
	for _, u := range tenantUsage {
		if start.After(u.Timestamp) == true {
			first++
		}

		if end.After(u.Timestamp) == true {
			last++
		}
	}

	return tenantUsage[first:last], nil
}

func reduceToZero(v int) int {
	if v < 0 {
		return 0
	}

	return v
}

func (ds *Datastore) addInstanceStats(stats []payloads.InstanceStat, nodeID string) error {
	for index := range stats {
		stat := stats[index]

		instanceStat := types.CiaoServerStats{
			ID:        stat.InstanceUUID,
			NodeID:    nodeID,
			Timestamp: time.Now(),
			Status:    stat.State,
			VCPUUsage: reduceToZero(stat.CPUUsage),
			MemUsage:  reduceToZero(stat.MemoryUsageMB),
			DiskUsage: reduceToZero(stat.DiskUsageMB),
		}

		ds.instanceLastStatLock.Lock()

		lastInstanceStat := ds.instanceLastStat[stat.InstanceUUID]

		deltaUsage := types.CiaoUsage{
			VCPU:   instanceStat.VCPUUsage - lastInstanceStat.VCPUUsage,
			Memory: instanceStat.MemUsage - lastInstanceStat.MemUsage,
			Disk:   instanceStat.DiskUsage - lastInstanceStat.DiskUsage,
		}

		ds.updateTenantUsage(deltaUsage, lastInstanceStat.TenantID)

		instanceStat.TenantID = lastInstanceStat.TenantID

		delete(ds.instanceLastStat, stat.InstanceUUID)
		ds.instanceLastStat[stat.InstanceUUID] = instanceStat

		ds.instanceLastStatLock.Unlock()

		ds.instancesLock.Lock()
		instance, ok := ds.instances[stat.InstanceUUID]
		if ok {
			instance.State = stat.State
			instance.NodeID = nodeID
			instance.SSHIP = stat.SSHIP
			instance.SSHPort = stat.SSHPort
			ds.nodesLock.Lock()
			ds.nodes[nodeID].instances[instance.ID] = instance
			ds.nodesLock.Unlock()
		}
		ds.instancesLock.Unlock()
	}

	return errors.Wrapf(ds.db.addInstanceStats(stats, nodeID), "error adding instance stats to database")
}

// GetTenantCNCISummary retrieves information about a given CNCI id, or all CNCIs
// If the cnci string is the null string, then this function will retrieve all
// tenants.  If cnci is not null, it will only provide information about a specific
// cnci.
func (ds *Datastore) GetTenantCNCISummary(cnciID string) ([]types.TenantCNCI, error) {
	var cncis []types.TenantCNCI

	instances, err := ds.GetAllCNCIInstances()
	if err != nil {
		return cncis, err
	}

	for _, i := range instances {
		if cnciID != "" && cnciID != i.ID {
			continue
		}

		cnci := types.TenantCNCI{
			TenantID:   i.TenantID,
			IPAddress:  i.IPAddress,
			MACAddress: i.MACAddress,
			InstanceID: i.ID,
		}

		cnci.Subnets = append(cnci.Subnets, i.Subnet)

		cncis = append(cncis, cnci)

		if cnciID != "" {
			break
		}
	}

	return cncis, nil
}

// GetCNCIWorkloadID returns the UUID of the workload template
// for the CNCI workload
func (ds *Datastore) GetCNCIWorkloadID() (string, error) {
	if ds.cnciWorkload.ID == "" {
		return "", errors.New("No CNCI Workload in datastore")
	}

	return ds.cnciWorkload.ID, nil
}

// GetNodeSummary provides a summary the state and count of instances running per node.
func (ds *Datastore) GetNodeSummary() ([]*types.NodeSummary, error) {
	var nodes []*types.NodeSummary

	ds.nodesLock.RLock()

	for _, n := range ds.nodes {
		var summary types.NodeSummary

		// count the total instances, no CNCI included
		for _, i := range n.instances {
			if i.CNCI == true {
				continue
			}

			summary.TotalInstances++

			switch i.State {
			case payloads.Pending:
				summary.TotalPendingInstances++
			case payloads.Running:
				summary.TotalRunningInstances++
			case payloads.Exited:
				summary.TotalPausedInstances++
			}
		}

		summary.NodeID = n.ID
		summary.TotalFailures = n.TotalFailures

		nodes = append(nodes, &summary)
	}

	ds.nodesLock.RUnlock()

	return nodes, nil
}

// GetBatchFrameSummary will retieve the count of traces we have for a specific label
func (ds *Datastore) GetBatchFrameSummary() ([]types.BatchFrameSummary, error) {
	// until we start caching frame stats, we have to send this
	// right through to the database.
	return ds.db.getBatchFrameSummary()
}

// GetBatchFrameStatistics will show individual trace data per instance for a batch of trace data.
// The batch is identified by the label.
func (ds *Datastore) GetBatchFrameStatistics(label string) ([]types.BatchFrameStat, error) {
	// until we start caching frame stats, we have to send this
	// right through to the database.
	return ds.db.getBatchFrameStatistics(label)
}

// GetEventLog retrieves all the log entries stored in the datastore.
func (ds *Datastore) GetEventLog() ([]*types.LogEntry, error) {
	// we don't as of yet cache any of the events that are logged.
	return ds.db.getEventLog()
}

// ClearLog will remove all the event entries from the event log
func (ds *Datastore) ClearLog() error {
	// we don't as of yet cache any of the events that are logged.
	return ds.db.clearLog()
}

// LogEvent will add a message to the persistent event log.
func (ds *Datastore) LogEvent(tenant string, msg string) error {
	e := types.LogEntry{
		TenantID:  tenant,
		EventType: string(userInfo),
		Message:   msg,
	}
	return ds.db.logEvent(e)
}

// LogError will add a message to the persistent event log as an error
func (ds *Datastore) LogError(tenant string, msg string) error {
	e := types.LogEntry{
		TenantID:  tenant,
		EventType: string(userError),
		Message:   msg,
	}
	return ds.db.logEvent(e)
}

// AddBlockDevice will store information about new BlockData into
// the datastore.
func (ds *Datastore) AddBlockDevice(device types.Volume) error {
	ds.bdLock.Lock()
	_, update := ds.blockDevices[device.ID]
	ds.bdLock.Unlock()

	// store persistently
	var err error
	if !update {
		err = errors.Wrap(ds.db.addBlockData(device), "Error adding block data to database")
	} else {
		err = errors.Wrap(ds.db.updateBlockData(device), "Error updating block data in database")
	}

	if err != nil {
		return err
	}

	ds.bdLock.Lock()
	ds.blockDevices[device.ID] = device
	ds.bdLock.Unlock()

	// update tenants cache
	ds.tenantsLock.Lock()
	devices := ds.tenants[device.TenantID].devices
	devices[device.ID] = device
	ds.tenantsLock.Unlock()
	return nil
}

// DeleteBlockDevice will delete a volume from the datastore.
// It also deletes it from the tenant's list of devices.
func (ds *Datastore) DeleteBlockDevice(ID string) error {
	ds.bdLock.Lock()
	dev, ok := ds.blockDevices[ID]
	if !ok {

		ds.bdLock.Unlock()
		return ErrNoBlockData
	}
	ds.bdLock.Unlock()

	err := errors.Wrap(ds.db.deleteBlockData(ID), "Error deleting block data from database")
	if err != nil {
		return err
	}

	ds.bdLock.Lock()
	ds.tenantsLock.Lock()

	delete(ds.blockDevices, ID)
	delete(ds.tenants[dev.TenantID].devices, ID)

	ds.tenantsLock.Unlock()
	ds.bdLock.Unlock()

	return nil
}

// GetBlockDevices will return all the BlockDevices associated with a tenant.
func (ds *Datastore) GetBlockDevices(tenant string) ([]types.Volume, error) {
	var devices []types.Volume

	ds.tenantsLock.RLock()

	_, ok := ds.tenants[tenant]
	if !ok {
		ds.tenantsLock.RUnlock()
		return devices, ErrNoTenant
	}

	for _, value := range ds.tenants[tenant].devices {
		devices = append(devices, value)
	}

	ds.tenantsLock.RUnlock()

	return devices, nil

}

// GetBlockDevice will return information about a block device from the
// datastore.
func (ds *Datastore) GetBlockDevice(ID string) (types.Volume, error) {
	ds.bdLock.RLock()
	data, ok := ds.blockDevices[ID]
	ds.bdLock.RUnlock()

	if !ok {
		return types.Volume{}, ErrNoBlockData
	}
	return data, nil
}

// UpdateBlockDevice will replace existing information about a block device
// in the datastore.
func (ds *Datastore) UpdateBlockDevice(data types.Volume) error {
	ds.bdLock.RLock()
	_, ok := ds.blockDevices[data.ID]
	ds.bdLock.RUnlock()

	if !ok {
		return ErrNoBlockData
	}

	return errors.Wrapf(ds.AddBlockDevice(data), "error updating block device (%v)", data.ID)
}

// CreateStorageAttachment will associate an instance with a block device in
// the datastore
func (ds *Datastore) CreateStorageAttachment(instanceID string, volume payloads.StorageResource) (types.StorageAttachment, error) {
	link := attachment{
		instanceID: instanceID,
		volumeID:   volume.ID,
	}

	a := types.StorageAttachment{
		InstanceID: instanceID,
		ID:         uuid.Generate().String(),
		BlockID:    volume.ID,
		Ephemeral:  volume.Ephemeral,
		Boot:       volume.Bootable,
	}

	err := ds.db.addStorageAttachment(a)
	if err != nil {
		return types.StorageAttachment{}, errors.Wrap(err, "error adding storage attachment to database")
	}

	// ensure that the volume is marked in use as we have created an attachment
	bd, err := ds.GetBlockDevice(volume.ID)
	if err != nil {
		_ = ds.db.deleteStorageAttachment(a.ID)
		return types.StorageAttachment{}, errors.Wrapf(err, "error fetching block device (%v)", volume.ID)
	}

	bd.State = types.InUse
	err = ds.UpdateBlockDevice(bd)
	if err != nil {
		_ = ds.db.deleteStorageAttachment(a.ID)
		return types.StorageAttachment{}, errors.Wrapf(err, "error updating block device (%v)", volume.ID)
	}

	// add it to our links map
	ds.attachLock.Lock()
	ds.attachments[a.ID] = a
	ds.instanceVolumes[link] = a.ID
	ds.attachLock.Unlock()

	return a, nil
}

// GetStorageAttachments returns a list of volumes associated with this instance.
func (ds *Datastore) GetStorageAttachments(instanceID string) []types.StorageAttachment {
	var links []types.StorageAttachment

	ds.attachLock.RLock()
	for _, a := range ds.attachments {
		if a.InstanceID == instanceID {
			links = append(links, a)
		}
	}
	ds.attachLock.RUnlock()

	return links
}

func (ds *Datastore) updateStorageAttachments(instanceID string) {
	ds.attachLock.Lock()

	// check to see if all the attachments we already
	// know about are in the list.
	for _, ID := range ds.instanceVolumes {
		a := ds.attachments[ID]

		if a.InstanceID == instanceID {
			bd, err := ds.GetBlockDevice(a.BlockID)
			if err != nil {
				glog.Warningf("error fetching block device (%v): %v", a.BlockID, err)
				continue
			}

			// update the state of the volume.
			bd.State = types.Available
			err = ds.UpdateBlockDevice(bd)
			if err != nil {
				glog.Warningf("error updating block device (%v): %v", a.BlockID, err)
			}

			// delete the attachment.
			key := attachment{
				instanceID: a.InstanceID,
				volumeID:   a.BlockID,
			}

			delete(ds.attachments, ID)
			delete(ds.instanceVolumes, key)

			// update persistent store asynch.
			// ok for lock to be held here, but
			// not needed as the db keeps it's
			// own locks.
			err = ds.db.deleteStorageAttachment(ID)
			if err != nil {
				glog.Warningf("error updating storage attachments: %v", err)
			}
		}
	}
	ds.attachLock.Unlock()
}

func (ds *Datastore) getStorageAttachment(instanceID string, volumeID string) (types.StorageAttachment, error) {
	var a types.StorageAttachment

	key := attachment{
		instanceID: instanceID,
		volumeID:   volumeID,
	}

	ds.attachLock.RLock()
	id, ok := ds.instanceVolumes[key]
	if ok {
		a = ds.attachments[id]
	}
	ds.attachLock.RUnlock()

	if !ok {
		return a, ErrNoStorageAttachment
	}

	return a, nil
}

// DeleteStorageAttachment will delete the attachment with the associated ID
// from the datastore.
func (ds *Datastore) DeleteStorageAttachment(ID string) error {
	err := errors.Wrapf(ds.db.deleteStorageAttachment(ID), "error deleting storage attachment (%v) from database", ID)
	if err != nil {
		return err
	}

	ds.attachLock.Lock()
	a, ok := ds.attachments[ID]
	if ok {
		key := attachment{
			instanceID: a.InstanceID,
			volumeID:   a.BlockID,
		}

		delete(ds.attachments, ID)
		delete(ds.instanceVolumes, key)
	}
	ds.attachLock.Unlock()

	if !ok {
		return ErrNoStorageAttachment
	}

	return nil
}

// GetVolumeAttachments will return a list of attachments associated with
// this volume ID.
func (ds *Datastore) GetVolumeAttachments(volume string) ([]types.StorageAttachment, error) {
	var attachments []types.StorageAttachment

	ds.attachLock.RLock()

	for _, a := range ds.attachments {
		if a.BlockID == volume {
			attachments = append(attachments, a)
		}
	}

	ds.attachLock.RUnlock()

	return attachments, nil
}

// GetPool will return an external IP Pool
func (ds *Datastore) GetPool(ID string) (types.Pool, error) {
	ds.poolsLock.RLock()
	p, ok := ds.pools[ID]
	ds.poolsLock.RUnlock()

	if !ok {
		return p, types.ErrPoolNotFound
	}

	return p, nil
}

// GetPools will return a list of external IP Pools
func (ds *Datastore) GetPools() ([]types.Pool, error) {
	var pools []types.Pool

	ds.poolsLock.RLock()

	for _, p := range ds.pools {
		pools = append(pools, p)
	}

	ds.poolsLock.RUnlock()

	return pools, nil
}

// lock for the map must be held by caller.
func (ds *Datastore) isDuplicateSubnet(new *net.IPNet) bool {
	for s, exists := range ds.externalSubnets {
		if exists == true {
			// this will always succeed
			_, subnet, _ := net.ParseCIDR(s)

			if subnet.Contains(new.IP) || new.Contains(subnet.IP) {
				return true
			}
		}
	}

	return false
}

// lock for the map must be held by the caller
func (ds *Datastore) isDuplicateIP(new net.IP) bool {
	// first make sure the IP isn't covered by a subnet
	for s, exists := range ds.externalSubnets {
		// this will always succeed
		_, subnet, _ := net.ParseCIDR(s)

		if exists == true {
			if subnet.Contains(new) {
				return true
			}
		}
	}

	// next make sure that the IP isn't already in a
	// different pool
	return ds.externalIPs[new.String()]
}

// AddPool will add a brand new pool to our datastore.
func (ds *Datastore) AddPool(pool types.Pool) error {
	ds.poolsLock.Lock()

	if len(pool.Subnets) > 0 {
		// check each one to make sure it's not in use.
		for _, subnet := range pool.Subnets {
			_, newSubnet, err := net.ParseCIDR(subnet.CIDR)
			if err != nil {
				ds.poolsLock.Unlock()
				return errors.Wrapf(err, "unable to parse subnet CIDR (%v)", subnet.CIDR)
			}

			if ds.isDuplicateSubnet(newSubnet) {
				ds.poolsLock.Unlock()
				return types.ErrDuplicateSubnet
			}

			// update our list of used subnets
			ds.externalSubnets[subnet.CIDR] = true
		}
	} else if len(pool.IPs) > 0 {
		var newIPs []net.IP

		// make sure valid and not duplicate
		for _, newIP := range pool.IPs {
			IP := net.ParseIP(newIP.Address)
			if IP == nil {
				ds.poolsLock.Unlock()
				return types.ErrInvalidIP
			}

			if ds.isDuplicateIP(IP) {
				ds.poolsLock.Unlock()
				return types.ErrDuplicateIP
			}

			newIPs = append(newIPs, IP)
		}

		// now that the whole list is confirmed, we can update
		for _, IP := range newIPs {
			ds.externalIPs[IP.String()] = true
		}
	}

	ds.pools[pool.ID] = pool
	err := ds.db.addPool(pool)

	ds.poolsLock.Unlock()

	if err != nil {
		// lock must not be held when calling.
		_ = ds.DeletePool(pool.ID)
	}

	return errors.Wrap(err, "error adding pool to database")
}

// DeletePool will delete an unused pool from our datastore.
func (ds *Datastore) DeletePool(ID string) error {
	ds.poolsLock.Lock()
	defer ds.poolsLock.Unlock()

	p, ok := ds.pools[ID]
	if !ok {
		return types.ErrPoolNotFound
	}

	// make sure all ips in this pool are not used.
	if p.Free != p.TotalIPs {
		return types.ErrPoolNotEmpty
	}

	// delete from persistent store
	err := errors.Wrapf(ds.db.deletePool(ID), "error deleting pool (%v) from database", ID)

	// delete all subnets
	for _, subnet := range p.Subnets {
		delete(ds.externalSubnets, subnet.CIDR)
	}

	// delete any individual IPs
	for _, IP := range p.IPs {
		delete(ds.externalIPs, IP.Address)
	}

	// delete the whole pool
	delete(ds.pools, ID)

	return err
}

// AddExternalSubnet will add a new subnet to an existing pool.
func (ds *Datastore) AddExternalSubnet(poolID string, subnet string) error {
	sub := types.ExternalSubnet{
		ID:   uuid.Generate().String(),
		CIDR: subnet,
	}

	_, ipNet, err := net.ParseCIDR(subnet)
	if err != nil {
		return errors.Wrapf(err, "unable to parse subnet CIDR (%v)", subnet)
	}

	ds.poolsLock.Lock()
	defer ds.poolsLock.Unlock()

	p, ok := ds.pools[poolID]
	if !ok {
		return types.ErrPoolNotFound
	}

	if ds.isDuplicateSubnet(ipNet) {
		return types.ErrDuplicateSubnet
	}

	ones, bits := ipNet.Mask.Size()

	// intentionally do not support /32 here, user should add by IP address instead
	// deduct gateway and broadcast
	newIPs := (1 << uint32(bits-ones)) - 2
	if newIPs <= 0 {
		return types.ErrSubnetTooSmall
	}
	p.TotalIPs += newIPs
	p.Free += newIPs
	p.Subnets = append(p.Subnets, sub)

	err = ds.db.updatePool(p)
	if err != nil {
		return errors.Wrap(err, "error updating pool in database")
	}

	// we are committed now.
	ds.pools[poolID] = p
	ds.externalSubnets[sub.CIDR] = true

	return nil
}

// AddExternalIPs will add a list of individual IPs to an existing pool.
func (ds *Datastore) AddExternalIPs(poolID string, IPs []string) error {
	ds.poolsLock.Lock()
	defer ds.poolsLock.Unlock()

	p, ok := ds.pools[poolID]
	if !ok {
		return types.ErrPoolNotFound
	}

	// sort to allow duplicate detection in IPs
	sort.Strings(IPs)

	// make sure valid and not duplicate
	lastIP := ""
	for _, newIP := range IPs {
		if lastIP == newIP {
			return types.ErrDuplicateIP
		}

		IP := net.ParseIP(newIP)
		if IP == nil {
			return types.ErrInvalidIP
		}

		if ds.isDuplicateIP(IP) {
			return types.ErrDuplicateIP
		}

		ExtIP := types.ExternalIP{
			ID:      uuid.Generate().String(),
			Address: IP.String(),
		}

		p.TotalIPs++
		p.Free++
		p.IPs = append(p.IPs, ExtIP)
		lastIP = newIP
	}

	// update persistent store.
	err := ds.db.updatePool(p)
	if err != nil {
		return errors.Wrap(err, "error updating pool in database")
	}

	// update cache.
	for _, IP := range p.IPs {
		ds.externalIPs[IP.Address] = true
	}
	ds.pools[poolID] = p

	return nil
}

// DeleteSubnet will remove an unused subnet from an existing pool.
func (ds *Datastore) DeleteSubnet(poolID string, subnetID string) error {
	ds.poolsLock.Lock()
	defer ds.poolsLock.Unlock()

	p, ok := ds.pools[poolID]
	if !ok {
		return types.ErrPoolNotFound
	}

	for i, sub := range p.Subnets {
		if sub.ID != subnetID {
			continue
		}

		// this path will be taken only once.
		IP, ipNet, err := net.ParseCIDR(sub.CIDR)
		if err != nil {
			return errors.Wrapf(err, "unable to parse subnet CIDR (%v)", sub.CIDR)
		}

		// check each address in this subnet is not mapped.
		for IP := IP.Mask(ipNet.Mask); ipNet.Contains(IP); incrementIP(IP) {
			_, ok := ds.mappedIPs[IP.String()]
			if ok {
				return types.ErrPoolNotEmpty
			}
		}

		ones, bits := ipNet.Mask.Size()
		numIPs := (1 << uint32(bits-ones)) - 2
		p.TotalIPs -= numIPs
		p.Free -= numIPs
		p.Subnets = append(p.Subnets[:i], p.Subnets[i+1:]...)

		err = ds.db.updatePool(p)
		if err != nil {
			return errors.Wrap(err, "error updating pool in database")
		}

		delete(ds.externalSubnets, sub.CIDR)
		ds.pools[poolID] = p

		return nil
	}

	return types.ErrInvalidPoolAddress
}

// DeleteExternalIP will remove an individual IP address from a pool.
func (ds *Datastore) DeleteExternalIP(poolID string, addrID string) error {
	ds.poolsLock.Lock()
	defer ds.poolsLock.Unlock()

	p, ok := ds.pools[poolID]
	if !ok {
		return types.ErrPoolNotFound
	}

	for i, extIP := range p.IPs {
		if extIP.ID != addrID {
			continue
		}

		// this path will be taken only once.
		// check address is not mapped.
		_, ok := ds.mappedIPs[extIP.Address]
		if ok {
			return types.ErrPoolNotEmpty
		}

		p.TotalIPs--
		p.Free--
		p.IPs = append(p.IPs[:i], p.IPs[i+1:]...)

		err := ds.db.updatePool(p)
		if err != nil {
			return errors.Wrap(err, "error updating pool in database")
		}

		delete(ds.externalIPs, extIP.Address)
		ds.pools[poolID] = p

		return nil
	}

	return types.ErrInvalidPoolAddress
}

func incrementIP(IP net.IP) {
	for i := len(IP) - 1; i >= 0; i-- {
		IP[i]++
		if IP[i] > 0 {
			break
		}
	}
}

// GetMappedIPs will return a list of mapped external IPs by tenant.
func (ds *Datastore) GetMappedIPs(tenant *string) []types.MappedIP {
	var mappedIPs []types.MappedIP

	ds.poolsLock.RLock()
	defer ds.poolsLock.RUnlock()

	for _, m := range ds.mappedIPs {
		if tenant != nil {
			if m.TenantID != *tenant {
				continue
			}
		}
		mappedIPs = append(mappedIPs, m)
	}

	return mappedIPs
}

// GetMappedIP will return a MappedIP struct for the given address.
func (ds *Datastore) GetMappedIP(address string) (types.MappedIP, error) {
	ds.poolsLock.RLock()
	defer ds.poolsLock.RUnlock()

	m, ok := ds.mappedIPs[address]
	if !ok {
		return types.MappedIP{}, types.ErrAddressNotFound
	}

	return m, nil
}

// MapExternalIP will allocate an external IP to an instance from a given pool.
func (ds *Datastore) MapExternalIP(poolID string, instanceID string) (types.MappedIP, error) {
	var m types.MappedIP

	instance, err := ds.GetInstance(instanceID)
	if err != nil {
		return m, errors.Wrapf(err, "error getting instance (%v)", instanceID)
	}

	ds.poolsLock.Lock()
	defer ds.poolsLock.Unlock()

	pool, ok := ds.pools[poolID]
	if !ok {
		return m, types.ErrPoolNotFound
	}

	if pool.Free == 0 {
		return m, types.ErrPoolEmpty
	}

	// find a free IP address in any subnet.
	for _, sub := range pool.Subnets {
		IP, ipNet, err := net.ParseCIDR(sub.CIDR)
		if err != nil {
			return m, errors.Wrapf(err, "error parsing subnet CIDR (%v)", sub.CIDR)
		}

		initIP := IP.Mask(ipNet.Mask)

		// skip gateway
		incrementIP(initIP)

		// check each address in this subnet
		for IP := initIP; ipNet.Contains(IP); incrementIP(IP) {
			_, ok := ds.mappedIPs[IP.String()]
			if !ok {
				m.ID = uuid.Generate().String()
				m.ExternalIP = IP.String()
				m.InternalIP = instance.IPAddress
				m.InstanceID = instanceID
				m.TenantID = instance.TenantID
				m.PoolID = pool.ID
				m.PoolName = pool.Name

				pool.Free--

				err = ds.db.addMappedIP(m)
				if err != nil {
					return types.MappedIP{}, errors.Wrap(err, "error adding IP mapping to database")
				}
				ds.mappedIPs[IP.String()] = m

				err = ds.db.updatePool(pool)
				if err != nil {
					return types.MappedIP{}, errors.Wrap(err, "error updating pool in database")
				}

				ds.pools[poolID] = pool

				return m, nil
			}
		}
	}

	// we are still looking. Check our individual IPs
	for _, IP := range pool.IPs {
		_, ok := ds.mappedIPs[IP.Address]
		if !ok {
			m.ID = uuid.Generate().String()
			m.ExternalIP = IP.Address
			m.InternalIP = instance.IPAddress
			m.InstanceID = instanceID
			m.TenantID = instance.TenantID
			m.PoolID = pool.ID
			m.PoolName = pool.Name

			pool.Free--

			err = ds.db.addMappedIP(m)
			if err != nil {
				return types.MappedIP{}, errors.Wrap(err, "error adding IP mapping to database")
			}
			ds.mappedIPs[IP.Address] = m

			err = ds.db.updatePool(pool)
			if err != nil {
				return types.MappedIP{}, errors.Wrap(err, "error updating pool in database")
			}

			ds.pools[poolID] = pool

			return m, nil
		}
	}

	// if you got here you are out of luck. But you never should.
	glog.Warningf("Pool reports %d free addresses but none found", pool.Free)
	return m, types.ErrPoolEmpty
}

// UnMapExternalIP will stop associating a given address with an instance.
func (ds *Datastore) UnMapExternalIP(address string) error {
	ds.poolsLock.Lock()
	defer ds.poolsLock.Unlock()

	m, ok := ds.mappedIPs[address]
	if !ok {
		return types.ErrAddressNotFound
	}

	// get pool and update Free
	pool, ok := ds.pools[m.PoolID]
	if !ok {
		return types.ErrPoolNotFound
	}

	pool.Free++

	err := ds.db.deleteMappedIP(m.ID)
	if err != nil {
		return errors.Wrap(err, "error deleting IP mapping from database")
	}
	delete(ds.mappedIPs, address)

	err = ds.db.updatePool(pool)
	if err != nil {
		return errors.Wrap(err, "error updating pool in database")
	}

	ds.pools[pool.ID] = pool

	return nil
}

// GenerateCNCIWorkload is used to create a workload definition for the CNCI.
// This function should be called prior to any workload launch.
func (ds *Datastore) GenerateCNCIWorkload(vcpus int, memMB int, diskMB int, key string) {
	// generate the CNCI workload.
	config := `---
#cloud-config
users:
  - name: cloud-admin
    gecos: CIAO Cloud Admin
    lock-passwd: true
    sudo: ALL=(ALL) NOPASSWD:ALL
    ssh-authorized-keys:
    - ` + key + `
...
`

	storage := types.StorageResource{
		ID:         "",
		Bootable:   true,
		Ephemeral:  true,
		SourceType: types.ImageService,
		Source:     "4e16e743-265a-4bf2-9fd1-57ada0b28904",
		Internal:   true,
	}

	wl := types.Workload{
		ID:          uuid.Generate().String(),
		Description: "CNCI",
		FWType:      string(payloads.EFI),
		VMType:      payloads.QEMU,
		Config:      config,
		Requirements: payloads.WorkloadRequirements{
			VCPUs:       vcpus,
			MemMB:       memMB,
			NetworkNode: true,
		},
		Storage:    []types.StorageResource{storage},
		Visibility: types.Internal,
	}

	// for now we have a single global cnci workload.
	ds.cnciWorkload = wl
}

// GetQuotas returns the set of quotas from the database without any caching.
func (ds *Datastore) GetQuotas(tenantID string) ([]types.QuotaDetails, error) {
	return ds.db.getQuotas(tenantID)
}

// UpdateQuotas updates the quotas for a tenant in the database.
func (ds *Datastore) UpdateQuotas(tenantID string, qds []types.QuotaDetails) error {
	return ds.db.updateQuotas(tenantID, qds)
}

// ResolveInstance maps an instance name to an uuid, returning "" if not found
// TODO: Replace this O(n) algorithm with another name to id map.
func (ds *Datastore) ResolveInstance(tenantID string, name string) (string, error) {
	ds.tenantsLock.RLock()
	defer ds.tenantsLock.RUnlock()

	t, ok := ds.tenants[tenantID]
	if !ok {
		return "", fmt.Errorf("Tenant not found: %s", tenantID)
	}

	for _, i := range t.instances {
		if i.Name == name || i.ID == name {
			return i.ID, nil
		}
	}

	return "", nil
}

// AddImage adds an image to the datastore and database
func (ds *Datastore) AddImage(i types.Image) error {
	ds.imageLock.Lock()
	defer ds.imageLock.Unlock()
	if _, ok := ds.images[i.ID]; ok {
		return api.ErrAlreadyExists
	}

	_, err := ds.ResolveImage(i.TenantID, i.Name)
	if err == nil {
		return api.ErrAlreadyExists
	} else if err != api.ErrNoImage {
		return err
	}

	_, err = ds.ResolveImage(i.TenantID, i.ID)
	if err == nil {
		return api.ErrAlreadyExists
	} else if err != api.ErrNoImage {
		return err
	}

	err = ds.db.updateImage(i)
	if err != nil {
		return errors.Wrap(err, "Unable to add image to database")
	}

	if i.TenantID != "" {
		ds.tenantsLock.Lock()
		if _, ok := ds.tenants[i.TenantID]; !ok {
			ds.tenantsLock.Unlock()
			_ = ds.db.deleteImage(i.ID)
			return types.ErrTenantNotFound
		}

		ds.tenants[i.TenantID].images = append(ds.tenants[i.TenantID].images, i.ID)
		ds.tenantsLock.Unlock()
	}

	ds.images[i.ID] = i

	if i.Visibility == types.Public {
		ds.publicImages = append(ds.publicImages, i.ID)
	}

	if i.Visibility == types.Internal {
		ds.internalImages = append(ds.internalImages, i.ID)
	}

	return nil
}

// UpdateImage updates the image metadate in the datastore and database
func (ds *Datastore) UpdateImage(i types.Image) error {
	ds.imageLock.Lock()
	defer ds.imageLock.Unlock()

	oldImage, ok := ds.images[i.ID]
	if !ok {
		return api.ErrNoImage
	}

	if oldImage.TenantID != i.TenantID ||
		oldImage.Visibility != i.Visibility {
		return errors.New("Changing visibility or tenant for image not permitted")
	}

	if oldImage.Name != i.Name {
		_, err := ds.ResolveImage(i.TenantID, i.Name)
		if err == nil {
			return api.ErrAlreadyExists
		} else if err != api.ErrNoImage {
			return err
		}
	}

	if err := ds.db.updateImage(i); err != nil {
		return errors.Wrap(err, "Error updating image in database")
	}

	ds.images[i.ID] = i

	return nil
}

// GetImage retrieves an image by ID
func (ds *Datastore) GetImage(ID string) (types.Image, error) {
	ds.imageLock.RLock()
	defer ds.imageLock.RUnlock()

	image, ok := ds.images[ID]
	if !ok {
		return types.Image{}, api.ErrNoImage
	}

	return image, nil
}

// ResolveImage retrieves an image by name or ID
func (ds *Datastore) ResolveImage(tenantID string, name string) (string, error) {
	ds.tenantsLock.RLock()
	defer ds.tenantsLock.RUnlock()

	if tenantID != "" && tenantID != "admin" {
		t, ok := ds.tenants[tenantID]
		if !ok {
			return "", fmt.Errorf("Tenant not found: %s", tenantID)
		}

		for _, id := range t.images {
			i := ds.images[id]
			if i.Name == name || i.ID == name {
				return i.ID, nil
			}
		}
	}

	for _, id := range ds.publicImages {
		i := ds.images[id]
		if i.Name == name || i.ID == name {
			return i.ID, nil
		}
	}

	for _, id := range ds.internalImages {
		i := ds.images[id]
		if i.Name == name || i.ID == name {
			return i.ID, nil
		}
	}

	return "", api.ErrNoImage
}

// GetImages obtains the images available for the optional tenantID/admin combo
func (ds *Datastore) GetImages(tenantID string, admin bool) ([]types.Image, error) {
	ds.imageLock.RLock()
	defer ds.imageLock.RUnlock()

	images := []types.Image{}

	if tenantID != "" {
		ds.tenantsLock.RLock()
		if _, ok := ds.tenants[tenantID]; !ok {
			ds.tenantsLock.RUnlock()
			return images, types.ErrTenantNotFound
		}

		for _, id := range ds.tenants[tenantID].images {
			images = append(images, ds.images[id])
		}

		ds.tenantsLock.RUnlock()
	}

	if admin {
		for _, id := range ds.internalImages {
			images = append(images, ds.images[id])
		}
	}

	for _, id := range ds.publicImages {
		images = append(images, ds.images[id])
	}

	return images, nil
}

// DeleteImage deleted the image from the datastore and the database
func (ds *Datastore) DeleteImage(ID string) error {
	ds.imageLock.Lock()
	defer ds.imageLock.Unlock()

	image, ok := ds.images[ID]
	if !ok {
		return api.ErrNoImage
	}

	if image.TenantID != "" {
		ds.tenantsLock.Lock()

		tenant, ok := ds.tenants[image.TenantID]
		if !ok {
			ds.tenantsLock.Unlock()
			return types.ErrTenantNotFound
		}

		for i, id := range tenant.images {
			if id == ID {
				tenant.images = append(tenant.images[:i], tenant.images[i+1:]...)
				break
			}
		}

		ds.tenantsLock.Unlock()
	}

	if image.Visibility == types.Internal {
		for i, id := range ds.internalImages {
			if id == ID {
				ds.internalImages = append(ds.internalImages[:i], ds.internalImages[i+1:]...)
				break
			}
		}
	}

	if image.Visibility == types.Public {
		for i, id := range ds.publicImages {
			if id == ID {
				ds.publicImages = append(ds.publicImages[:i], ds.publicImages[i+1:]...)
				break
			}
		}
	}

	delete(ds.images, ID)

	return nil
}
