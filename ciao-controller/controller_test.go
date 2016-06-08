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

package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	datastore "github.com/01org/ciao/ciao-controller/internal/datastore"
	"github.com/01org/ciao/ciao-controller/types"
	"github.com/01org/ciao/payloads"
	"github.com/01org/ciao/ssntp"
	"github.com/01org/ciao/testutil"
	"github.com/docker/distribution/uuid"
)

func addTestTenant() (tenant *types.Tenant, err error) {
	/* add a new tenant */
	tuuid := uuid.Generate()
	tenant, err = context.ds.AddTenant(tuuid.String())
	if err != nil {
		return
	}

	// Add fake CNCI
	err = context.ds.AddTenantCNCI(tuuid.String(), uuid.Generate().String(), tenant.CNCIMAC)
	if err != nil {
		return
	}
	err = context.ds.AddCNCIIP(tenant.CNCIMAC, "192.168.0.1")
	if err != nil {
		return
	}
	return
}

func addComputeTestTenant() (tenant *types.Tenant, err error) {
	/* add a new tenant */
	tenant, err = context.ds.AddTenant(computeTestUser)
	if err != nil {
		return
	}

	// Add fake CNCI
	err = context.ds.AddTenantCNCI(computeTestUser, uuid.Generate().String(), tenant.CNCIMAC)
	if err != nil {
		return
	}

	err = context.ds.AddCNCIIP(tenant.CNCIMAC, "192.168.0.2")
	if err != nil {
		return
	}

	return
}

func BenchmarkStartSingleWorkload(b *testing.B) {
	var err error

	/* add a new tenant */
	tuuid := uuid.Generate()
	tenant, err := context.ds.AddTenant(tuuid.String())
	if err != nil {
		b.Error(err)
	}

	// Add fake CNCI
	err = context.ds.AddTenantCNCI(tuuid.String(), uuid.Generate().String(), tenant.CNCIMAC)
	if err != nil {
		b.Error(err)
	}
	err = context.ds.AddCNCIIP(tenant.CNCIMAC, "192.168.0.1")
	if err != nil {
		b.Error(err)
	}

	// get workload ID
	wls, err := context.ds.GetWorkloads()
	if err != nil || len(wls) == 0 {
		b.Fatal(err)
	}

	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		_, err = context.startWorkload(wls[0].ID, tuuid.String(), 1, false, "")
		if err != nil {
			b.Error(err)
		}
	}
}

func BenchmarkStart1000Workload(b *testing.B) {
	var err error

	/* add a new tenant */
	tuuid := uuid.Generate()
	tenant, err := context.ds.AddTenant(tuuid.String())
	if err != nil {
		b.Error(err)
	}

	// Add fake CNCI
	err = context.ds.AddTenantCNCI(tuuid.String(), uuid.Generate().String(), tenant.CNCIMAC)
	if err != nil {
		b.Error(err)
	}
	err = context.ds.AddCNCIIP(tenant.CNCIMAC, "192.168.0.1")
	if err != nil {
		b.Error(err)
	}

	// get workload ID
	wls, err := context.ds.GetWorkloads()
	if err != nil || len(wls) == 0 {
		b.Fatal(err)
	}

	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		_, err = context.startWorkload(wls[0].ID, tuuid.String(), 1000, false, "")
		if err != nil {
			b.Error(err)
		}
	}
}

func BenchmarkNewConfig(b *testing.B) {
	var err error

	tenant, err := addTestTenant()
	if err != nil {
		b.Error(err)
	}

	// get workload ID
	wls, err := context.ds.GetWorkloads()
	if err != nil || len(wls) == 0 {
		b.Fatal(err)
	}

	id := uuid.Generate()

	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		_, err := newConfig(context, wls[0], id.String(), tenant.ID)
		if err != nil {
			b.Error(err)
		}
	}
}

func TestTenantWithinBounds(t *testing.T) {
	var err error

	tenant, err := addTestTenant()
	if err != nil {
		t.Fatal(err)
	}

	/* put tenant limit of 1 instance */
	err = context.ds.AddLimit(tenant.ID, 1, 1)
	if err != nil {
		t.Fatal(err)
	}

	wls, err := context.ds.GetWorkloads()
	if err != nil || len(wls) == 0 {
		t.Fatal(err)
	}

	_, err = context.startWorkload(wls[0].ID, tenant.ID, 1, false, "")
	if err != nil {
		t.Fatal(err)
	}
}

func TestTenantOutOfBounds(t *testing.T) {
	var err error

	/* add a new tenant */
	tenant, err := addTestTenant()
	if err != nil {
		t.Error(err)
	}

	/* put tenant limit of 1 instance */
	_ = context.ds.AddLimit(tenant.ID, 1, 1)

	wls, err := context.ds.GetWorkloads()
	if err != nil || len(wls) == 0 {
		t.Fatal(err)
	}

	/* try to send 2 workload start commands */
	_, err = context.startWorkload(wls[0].ID, tenant.ID, 2, false, "")
	if err == nil {
		t.Errorf("Not tracking limits correctly")
	}
}

// TestNewTenantHardwareAddr
// Confirm that the mac addresses generated from a given
// IP address is as expected.
func TestNewTenantHardwareAddr(t *testing.T) {
	ip := net.ParseIP("172.16.0.2")
	expectedMAC := "02:00:ac:10:00:02"
	hw := newTenantHardwareAddr(ip)
	if hw.String() != expectedMAC {
		t.Error("Expected: ", expectedMAC, " Received: ", hw.String())
	}
}

func TestStartWorkload(t *testing.T) {
	var reason payloads.StartFailureReason

	client, _ := testStartWorkload(t, 1, false, reason)
	defer client.Ssntp.Close()
}

func TestStartTracedWorkload(t *testing.T) {
	client := testStartTracedWorkload(t)
	defer client.Ssntp.Close()
}

func TestStartWorkloadLaunchCNCI(t *testing.T) {
	netClient, instances := testStartWorkloadLaunchCNCI(t, 1)
	defer netClient.Ssntp.Close()

	id := instances[0].TenantID

	tenant, err := context.ds.GetTenant(id)
	if err != nil {
		t.Fatal(err)
	}

	if tenant.CNCIIP == "" {
		t.Fatal("CNCI Info not updated")
	}

}

// TBD: for the launch CNCI tests, I really need to create a fake
// network node and test that way.

func TestDeleteInstance(t *testing.T) {
	var reason payloads.StartFailureReason

	client, instances := testStartWorkload(t, 1, false, reason)
	defer client.Ssntp.Close()

	time.Sleep(1 * time.Second)

	client.SendStats()

	c := server.AddCmdChan(ssntp.DELETE)

	time.Sleep(1 * time.Second)

	err := context.deleteInstance(instances[0].ID)
	if err != nil {
		t.Fatal(err)
	}

	result, err := server.GetCmdChanResult(c, ssntp.DELETE)
	if err != nil {
		t.Fatal(err)
	}
	if result.InstanceUUID != instances[0].ID {
		t.Fatal("Did not get correct Instance ID")
	}
}

func TestStopInstance(t *testing.T) {
	var reason payloads.StartFailureReason

	client, instances := testStartWorkload(t, 1, false, reason)
	defer client.Ssntp.Close()

	time.Sleep(1 * time.Second)

	client.SendStats()

	c := server.AddCmdChan(ssntp.STOP)

	time.Sleep(1 * time.Second)

	err := context.stopInstance(instances[0].ID)
	if err != nil {
		t.Fatal(err)
	}

	result, err := server.GetCmdChanResult(c, ssntp.STOP)
	if err != nil {
		t.Fatal(err)
	}
	if result.InstanceUUID != instances[0].ID {
		t.Fatal("Did not get correct Instance ID")
	}
}

func TestRestartInstance(t *testing.T) {
	var reason payloads.StartFailureReason

	client, instances := testStartWorkload(t, 1, false, reason)
	defer client.Ssntp.Close()

	time.Sleep(1 * time.Second)

	client.SendStats()

	c := server.AddCmdChan(ssntp.STOP)

	time.Sleep(1 * time.Second)

	err := context.stopInstance(instances[0].ID)
	if err != nil {
		t.Fatal(err)
	}

	result, err := server.GetCmdChanResult(c, ssntp.STOP)
	if err != nil {
		t.Fatal(err)
	}
	if result.InstanceUUID != instances[0].ID {
		t.Fatal("Did not get correct Instance ID")
	}

	// now attempt to restart
	time.Sleep(1 * time.Second)

	client.SendStats()

	c = server.AddCmdChan(ssntp.RESTART)

	time.Sleep(1 * time.Second)

	err = context.restartInstance(instances[0].ID)
	if err != nil {
		t.Fatal(err)
	}

	result, err = server.GetCmdChanResult(c, ssntp.RESTART)
	if err != nil {
		t.Fatal(err)
	}
	if result.InstanceUUID != instances[0].ID {
		t.Fatal("Did not get correct Instance ID")
	}
}

func TestEvacuateNode(t *testing.T) {
	client, err := testutil.NewSsntpTestClientConnection("EvacuateNode", ssntp.AGENT, testutil.AgentUUID)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Ssntp.Close()

	c := server.AddCmdChan(ssntp.EVACUATE)

	// ok to not send workload first?

	err = context.evacuateNode(client.UUID)
	if err != nil {
		t.Error(err)
	}

	result, err := server.GetCmdChanResult(c, ssntp.EVACUATE)
	if err != nil {
		t.Fatal(err)
	}
	if result.NodeUUID != client.UUID {
		t.Fatal("Did not get node ID")
	}
}

func TestInstanceDeletedEvent(t *testing.T) {
	var reason payloads.StartFailureReason

	client, instances := testStartWorkload(t, 1, false, reason)
	defer client.Ssntp.Close()

	time.Sleep(1 * time.Second)

	client.SendStats()

	time.Sleep(1 * time.Second)

	err := context.deleteInstance(instances[0].ID)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(1 * time.Second)

	client.SendDeleteEvent(instances[0].ID)

	time.Sleep(1 * time.Second)

	// try to get instance info
	_, err = context.ds.GetInstance(instances[0].ID)
	if err == nil {
		t.Error("Instance not deleted")
	}
}

func TestLaunchCNCI(t *testing.T) {
	netClient, err := testutil.NewSsntpTestClientConnection("LaunchCNCI", ssntp.NETAGENT, testutil.NetAgentUUID)
	if err != nil {
		t.Fatal(err)
	}
	defer netClient.Ssntp.Close()

	c := server.AddCmdChan(ssntp.START)

	id := uuid.Generate().String()

	// this blocks till it get success or failure
	go context.addTenant(id)

	result, err := server.GetCmdChanResult(c, ssntp.START)
	if err != nil {
		t.Fatal(err)
	}
	if result.TenantUUID != id {
		t.Fatal("Did not get correct tenant ID")
	}
	if !result.CNCI {
		t.Fatal("this is not a CNCI launch request")
	}

	time.Sleep(2 * time.Second)

	tenant, err := context.ds.GetTenant(id)
	if err != nil || tenant == nil {
		t.Fatal(err)
	}

	if tenant.CNCIIP == "" {
		t.Fatal("CNCI Info not updated")
	}
}

func TestStartFailure(t *testing.T) {
	reason := payloads.FullCloud

	client, _ := testStartWorkload(t, 1, true, reason)
	defer client.Ssntp.Close()

	// since we had a start failure, we should confirm that the
	// instance is no longer pending in the database
}

func TestStopFailure(t *testing.T) {
	context.ds.ClearLog()

	var reason payloads.StartFailureReason

	client, instances := testStartWorkload(t, 1, false, reason)
	defer client.Ssntp.Close()

	client.StopFail = true
	client.StopFailReason = payloads.StopNoInstance

	time.Sleep(1 * time.Second)

	client.SendStats()

	time.Sleep(1 * time.Second)

	c := server.AddCmdChan(ssntp.STOP)

	time.Sleep(1 * time.Second)

	err := context.stopInstance(instances[0].ID)
	if err != nil {
		t.Fatal(err)
	}

	result, err := server.GetCmdChanResult(c, ssntp.STOP)
	if err != nil {
		t.Fatal(err)
	}
	if result.InstanceUUID != instances[0].ID {
		t.Fatal("Did not get correct Instance ID")
	}

	time.Sleep(1 * time.Second)

	// the response to a stop failure is to log the failure
	entries, err := context.ds.GetEventLog()
	if err != nil {
		t.Fatal(err)
	}

	expectedMsg := fmt.Sprintf("Stop Failure %s: %s", instances[0].ID, client.StopFailReason.String())

	for i := range entries {
		if entries[i].Message == expectedMsg {
			return
		}
	}
	t.Error("Did not find failure message in Log")
}

func TestRestartFailure(t *testing.T) {
	context.ds.ClearLog()

	var reason payloads.StartFailureReason

	client, instances := testStartWorkload(t, 1, false, reason)
	defer client.Ssntp.Close()

	client.RestartFail = true
	client.RestartFailReason = payloads.RestartLaunchFailure

	time.Sleep(1 * time.Second)

	client.SendStats()

	time.Sleep(1 * time.Second)

	c := server.AddCmdChan(ssntp.STOP)

	err := context.stopInstance(instances[0].ID)
	if err != nil {
		t.Fatal(err)
	}

	result, err := server.GetCmdChanResult(c, ssntp.STOP)
	if err != nil {
		t.Fatal(err)
	}
	if result.InstanceUUID != instances[0].ID {
		t.Fatal("Did not get correct Instance ID")
	}

	time.Sleep(1 * time.Second)

	client.SendStats()

	time.Sleep(1 * time.Second)

	c = server.AddCmdChan(ssntp.RESTART)

	err = context.restartInstance(instances[0].ID)
	if err != nil {
		t.Fatal(err)
	}

	result, err = server.GetCmdChanResult(c, ssntp.RESTART)
	if err != nil {
		t.Fatal(err)
	}
	if result.InstanceUUID != instances[0].ID {
		t.Fatal("Did not get correct Instance ID")
	}

	time.Sleep(1 * time.Second)

	// the response to a restart failure is to log the failure
	entries, err := context.ds.GetEventLog()
	if err != nil {
		t.Fatal(err)
	}

	expectedMsg := fmt.Sprintf("Restart Failure %s: %s", instances[0].ID, client.RestartFailReason.String())

	for i := range entries {
		if entries[i].Message == expectedMsg {
			return
		}
	}
	t.Error("Did not find failure message in Log")
}

func TestNoNetwork(t *testing.T) {
	nn := true

	noNetwork = &nn

	var reason payloads.StartFailureReason

	client, _ := testStartWorkload(t, 1, false, reason)
	defer client.Ssntp.Close()
}

// NOTE: the caller is responsible for calling Ssntp.Close() on the *SsntpTestClient
func testStartTracedWorkload(t *testing.T) *testutil.SsntpTestClient {
	tenant, err := addTestTenant()
	if err != nil {
		t.Fatal(err)
	}

	client, err := testutil.NewSsntpTestClientConnection("StartTracedWorkload", ssntp.AGENT, testutil.AgentUUID)
	if err != nil {
		t.Fatal(err)
	}
	// caller of TestStartTracedWorkload() owns doing the close
	//defer client.Ssntp.Close()

	wls, err := context.ds.GetWorkloads()
	if err != nil {
		t.Fatal(err)
	}
	if len(wls) == 0 {
		t.Fatal("No workloads, expected len(wls) > 0, got len(wls) == 0")
	}

	c := client.AddCmdChan(ssntp.START)

	instances, err := context.startWorkload(wls[0].ID, tenant.ID, 1, true, "testtrace1")
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 1 {
		t.Fatalf("Wrong number of instances, expected 1, got %d", len(instances))
	}

	result, err := client.GetCmdChanResult(c, ssntp.START)
	if err != nil {
		t.Fatal(err)
	}
	if result.InstanceUUID != instances[0].ID {
		t.Fatal("Did not get correct Instance ID")
	}

	return client
}

// NOTE: the caller is responsible for calling Ssntp.Close() on the *SsntpTestClient
func testStartWorkload(t *testing.T, num int, fail bool, reason payloads.StartFailureReason) (*testutil.SsntpTestClient, []*types.Instance) {
	tenant, err := addTestTenant()
	if err != nil {
		t.Fatal(err)
	}

	client, err := testutil.NewSsntpTestClientConnection("StartWorkload", ssntp.AGENT, testutil.AgentUUID)
	if err != nil {
		t.Fatal(err)
	}
	// caller of TestStartWorkload() owns doing the close
	//defer client.Ssntp.Close()

	wls, err := context.ds.GetWorkloads()
	if err != nil {
		t.Fatal(err)
	}
	if len(wls) == 0 {
		t.Fatal("No workloads, expected len(wls) > 0, got len(wls) == 0")
	}

	c := client.AddCmdChan(ssntp.START)
	client.StartFail = fail
	client.StartFailReason = reason

	instances, err := context.startWorkload(wls[0].ID, tenant.ID, num, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != num {
		t.Fatalf("Wrong number of instances, expected %d, got %d", len(instances), num)
	}

	result, err := client.GetCmdChanResult(c, ssntp.START)
	if err != nil {
		t.Fatal(err)
	}
	if result.InstanceUUID != instances[0].ID {
		t.Fatal("Did not get correct Instance ID")
	}

	return client, instances
}

// TestStartWorkloadLaunchCNCI starts a test CNCI
// NOTE: the caller is responsible for calling Ssntp.Close() on the *SsntpTestClient
func testStartWorkloadLaunchCNCI(t *testing.T, num int) (*testutil.SsntpTestClient, []*types.Instance) {
	netClient, err := testutil.NewSsntpTestClientConnection("StartWorkloadLaunchCNCI", ssntp.NETAGENT, testutil.NetAgentUUID)
	if err != nil {
		t.Fatal(err)
	}
	// caller of TestStartWorkloadLaunchCNCI() owns doing the close
	//defer netClient.Ssntp.Close()

	wls, err := context.ds.GetWorkloads()
	if err != nil {
		t.Fatal(err)
	}
	if len(wls) == 0 {
		t.Fatal("No workloads, expected len(wls) > 0, got len(wls) == 0")
	}

	c := server.AddCmdChan(ssntp.START)

	id := uuid.Generate().String()

	var instances []*types.Instance

	go func() {
		instances, err = context.startWorkload(wls[0].ID, id, 1, false, "")
		if err != nil {
			t.Fatal(err)
		}

		if len(instances) != 1 {
			t.Fatalf("Wrong number of instances, expected 1, got %d", len(instances))
		}
	}()

	result, err := server.GetCmdChanResult(c, ssntp.START)
	if err != nil {
		t.Fatal(err)
	}
	if result.TenantUUID != id {
		t.Fatal("Did not get correct tenant ID")
	}

	if !result.CNCI {
		t.Fatal("this is not a CNCI launch request")
	}

	c = server.AddCmdChan(ssntp.START)

	result, err = server.GetCmdChanResult(c, ssntp.START)
	if err != nil {
		t.Fatal(err)
	}
	if result.InstanceUUID != instances[0].ID {
		t.Fatal("Did not get correct Instance ID")
	}

	return netClient, instances
}

var testClients []*testutil.SsntpTestClient
var context *controller
var server testutil.SsntpTestServer
var computeURL string
var testIdentityURL string

const computeTestUser = "f452bbc7-5076-44d5-922c-3b9d2ce1503f"

func TestMain(m *testing.M) {
	flag.Parse()

	computeURL = "https://localhost:" + strconv.Itoa(computeAPIPort)

	// create fake ssntp server
	testutil.StartTestServer(&server)
	defer server.Ssntp.Stop()

	context = new(controller)
	context.ds = new(datastore.Datastore)

	dsConfig := datastore.Config{
		PersistentURI:     "./ciao-controller-test.db",
		TransientURI:      "./ciao-controller-test-tdb.db",
		InitTablesPath:    *tablesInitPath,
		InitWorkloadsPath: *workloadsPath,
	}

	err := context.ds.Init(dsConfig)
	if err != nil {
		os.Exit(1)
	}

	config := &ssntp.Config{
		URI:    "localhost",
		CAcert: *caCert,
		Cert:   *cert,
	}

	context.client, err = newSSNTPClient(context, config)
	if err != nil {
		os.Exit(1)
	}

	testIdentityConfig := testutil.IdentityConfig{
		ComputeURL: computeURL,
		ProjectID:  computeTestUser,
	}

	id := testutil.StartIdentityServer(testIdentityConfig)
	defer id.Close()

	idConfig := identityConfig{
		endpoint:        id.URL,
		serviceUserName: "test",
		servicePassword: "iheartciao",
	}

	testIdentityURL = id.URL

	context.id, err = newIdentityClient(idConfig)
	if err != nil {
		fmt.Println(err)
		// keep going anyway - any compute api tests will fail.
	}

	_, _ = addComputeTestTenant()
	go createComputeAPI(context)

	time.Sleep(1 * time.Second)

	code := m.Run()

	context.client.Disconnect()
	context.ds.Exit()

	os.Remove("./ciao-controller-test.db")
	os.Remove("./ciao-controller-test.db-shm")
	os.Remove("./ciao-controller-test.db-wal")
	os.Remove("./ciao-controller-test-tdb.db")
	os.Remove("./ciao-controller-test-tdb.db-shm")
	os.Remove("./ciao-controller-test-tdb.db-wal")

	os.Exit(code)
}
