// Copyright 2011, 2012, 2013 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package ec2_test

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	jc "github.com/juju/testing/checkers"
	"github.com/juju/utils"
	goyaml "gopkg.in/yaml.v1"
	"launchpad.net/goamz/aws"
	amzec2 "launchpad.net/goamz/ec2"
	"launchpad.net/goamz/ec2/ec2test"
	"launchpad.net/goamz/s3"
	"launchpad.net/goamz/s3/s3test"
	gc "launchpad.net/gocheck"

	"github.com/juju/juju/constraints"
	"github.com/juju/juju/environs"
	"github.com/juju/juju/environs/bootstrap"
	"github.com/juju/juju/environs/config"
	"github.com/juju/juju/environs/configstore"
	"github.com/juju/juju/environs/imagemetadata"
	"github.com/juju/juju/environs/jujutest"
	"github.com/juju/juju/environs/simplestreams"
	envtesting "github.com/juju/juju/environs/testing"
	"github.com/juju/juju/environs/tools"
	"github.com/juju/juju/instance"
	"github.com/juju/juju/juju/arch"
	"github.com/juju/juju/juju/testing"
	"github.com/juju/juju/network"
	"github.com/juju/juju/provider/common"
	"github.com/juju/juju/provider/ec2"
	coretesting "github.com/juju/juju/testing"
	"github.com/juju/juju/utils/ssh"
	"github.com/juju/juju/version"
)

type ProviderSuite struct {
	coretesting.BaseSuite
}

var _ = gc.Suite(&ProviderSuite{})

func (t *ProviderSuite) assertGetImageMetadataSources(c *gc.C, stream, officialSourcePath string) {
	// Make an env configured with the stream.
	envAttrs := localConfigAttrs
	if stream != "" {
		envAttrs = envAttrs.Merge(coretesting.Attrs{
			"image-stream": stream,
		})
	}
	cfg, err := config.New(config.NoDefaults, envAttrs)
	c.Assert(err, gc.IsNil)
	env, err := environs.Prepare(cfg, coretesting.Context(c), configstore.NewMem())
	c.Assert(err, gc.IsNil)
	c.Assert(env, gc.NotNil)

	sources, err := imagemetadata.GetMetadataSources(env)
	c.Assert(err, gc.IsNil)
	c.Assert(len(sources), gc.Equals, 2)
	var urls = make([]string, len(sources))
	for i, source := range sources {
		url, err := source.URL("")
		c.Assert(err, gc.IsNil)
		urls[i] = url
	}
	// The control bucket URL contains the bucket name.
	c.Check(strings.Contains(urls[0], ec2.ControlBucketName(env)+"/images"), jc.IsTrue)
	c.Assert(urls[1], gc.Equals, fmt.Sprintf("http://cloud-images.ubuntu.com/%s/", officialSourcePath))
}

func (t *ProviderSuite) TestGetImageMetadataSources(c *gc.C) {
	t.assertGetImageMetadataSources(c, "", "releases")
	t.assertGetImageMetadataSources(c, "released", "releases")
	t.assertGetImageMetadataSources(c, "daily", "daily")
}

var localConfigAttrs = coretesting.FakeConfig().Merge(coretesting.Attrs{
	"name":           "sample",
	"type":           "ec2",
	"region":         "test",
	"control-bucket": "test-bucket",
	"access-key":     "x",
	"secret-key":     "x",
	"agent-version":  version.Current.Number.String(),
})

func registerLocalTests() {
	// N.B. Make sure the region we use here
	// has entries in the images/query txt files.
	aws.Regions["test"] = aws.Region{
		Name: "test",
		Sign: aws.SignV2,
	}

	gc.Suite(&localServerSuite{})
	gc.Suite(&localLiveSuite{})
	gc.Suite(&localNonUSEastSuite{})
}

// localLiveSuite runs tests from LiveTests using a fake
// EC2 server that runs within the test process itself.
type localLiveSuite struct {
	LiveTests
	srv                localServer
	restoreEC2Patching func()
}

func (t *localLiveSuite) SetUpSuite(c *gc.C) {
	t.TestConfig = localConfigAttrs
	t.restoreEC2Patching = patchEC2ForTesting()
	t.srv.startServer(c)
	t.LiveTests.SetUpSuite(c)
}

func (t *localLiveSuite) TearDownSuite(c *gc.C) {
	t.LiveTests.TearDownSuite(c)
	t.srv.stopServer(c)
	t.restoreEC2Patching()
}

// localServer represents a fake EC2 server running within
// the test process itself.
type localServer struct {
	ec2srv *ec2test.Server
	s3srv  *s3test.Server
	config *s3test.Config
}

func (srv *localServer) startServer(c *gc.C) {
	var err error
	srv.ec2srv, err = ec2test.NewServer()
	if err != nil {
		c.Fatalf("cannot start ec2 test server: %v", err)
	}
	srv.s3srv, err = s3test.NewServer(srv.config)
	if err != nil {
		c.Fatalf("cannot start s3 test server: %v", err)
	}
	aws.Regions["test"] = aws.Region{
		Name:                 "test",
		EC2Endpoint:          srv.ec2srv.URL(),
		S3Endpoint:           srv.s3srv.URL(),
		S3LocationConstraint: true,
		Sign:                 aws.SignV2,
	}
	s3inst := s3.New(aws.Auth{}, aws.Regions["test"])
	storage := ec2.BucketStorage(s3inst.Bucket("juju-dist"))
	envtesting.UploadFakeTools(c, storage)
	srv.addSpice(c)

	zones := make([]amzec2.AvailabilityZoneInfo, 3)
	zones[0].Region = "test"
	zones[0].Name = "test-available"
	zones[0].State = "available"
	zones[1].Region = "test"
	zones[1].Name = "test-impaired"
	zones[1].State = "impaired"
	zones[2].Region = "test"
	zones[2].Name = "test-unavailable"
	zones[2].State = "unavailable"
	srv.ec2srv.SetAvailabilityZones(zones)
}

// addSpice adds some "spice" to the local server
// by adding state that may cause tests to fail.
func (srv *localServer) addSpice(c *gc.C) {
	states := []amzec2.InstanceState{
		ec2test.ShuttingDown,
		ec2test.Terminated,
		ec2test.Stopped,
	}
	for _, state := range states {
		srv.ec2srv.NewInstances(1, "m1.small", "ami-a7f539ce", state, nil)
	}
}

func (srv *localServer) stopServer(c *gc.C) {
	srv.ec2srv.Quit()
	srv.s3srv.Quit()
	// Clear out the region because the server address is
	// no longer valid.
	delete(aws.Regions, "test")
}

// localServerSuite contains tests that run against a fake EC2 server
// running within the test process itself.  These tests can test things that
// would be unreasonably slow or expensive to test on a live Amazon server.
// It starts a new local ec2test server for each test.  The server is
// accessed by using the "test" region, which is changed to point to the
// network address of the local server.
type localServerSuite struct {
	coretesting.BaseSuite
	jujutest.Tests
	srv                localServer
	restoreEC2Patching func()
}

func (t *localServerSuite) SetUpSuite(c *gc.C) {
	t.TestConfig = localConfigAttrs
	t.restoreEC2Patching = patchEC2ForTesting()
	t.BaseSuite.SetUpSuite(c)
}

func (t *localServerSuite) TearDownSuite(c *gc.C) {
	t.BaseSuite.TearDownSuite(c)
	t.restoreEC2Patching()
}

func (t *localServerSuite) SetUpTest(c *gc.C) {
	t.BaseSuite.SetUpTest(c)
	t.srv.startServer(c)
	t.Tests.SetUpTest(c)
	t.PatchValue(&version.Current, version.Binary{
		Number: version.Current.Number,
		Series: coretesting.FakeDefaultSeries,
		Arch:   arch.AMD64,
	})
}

func (t *localServerSuite) TearDownTest(c *gc.C) {
	t.Tests.TearDownTest(c)
	t.srv.stopServer(c)
	t.BaseSuite.TearDownTest(c)
}

func (t *localServerSuite) TestBootstrapInstanceUserDataAndState(c *gc.C) {
	env := t.Prepare(c)
	envtesting.UploadFakeTools(c, env.Storage())
	err := bootstrap.Bootstrap(coretesting.Context(c), env, environs.BootstrapParams{})
	c.Assert(err, gc.IsNil)

	// check that StateServerInstances returns the id of the bootstrap machine.
	instanceIds, err := env.StateServerInstances()
	c.Assert(err, gc.IsNil)
	c.Assert(instanceIds, gc.HasLen, 1)

	insts, err := env.AllInstances()
	c.Assert(err, gc.IsNil)
	c.Assert(insts, gc.HasLen, 1)
	c.Check(insts[0].Id(), gc.Equals, instanceIds[0])

	// check that the user data is configured to start zookeeper
	// and the machine and provisioning agents.
	// check that the user data is configured to only configure
	// authorized SSH keys and set the log output; everything
	// else happens after the machine is brought up.
	inst := t.srv.ec2srv.Instance(string(insts[0].Id()))
	c.Assert(inst, gc.NotNil)
	addresses, err := insts[0].Addresses()
	c.Assert(err, gc.IsNil)
	c.Assert(addresses, gc.Not(gc.HasLen), 0)
	userData, err := utils.Gunzip(inst.UserData)
	c.Assert(err, gc.IsNil)
	c.Logf("first instance: UserData: %q", userData)
	var userDataMap map[interface{}]interface{}
	err = goyaml.Unmarshal(userData, &userDataMap)
	c.Assert(err, gc.IsNil)
	c.Assert(userDataMap, jc.DeepEquals, map[interface{}]interface{}{
		"output": map[interface{}]interface{}{
			"all": "| tee -a /var/log/cloud-init-output.log",
		},
		"ssh_authorized_keys": splitAuthKeys(env.Config().AuthorizedKeys()),
		"runcmd": []interface{}{
			"set -xe",
			"install -D -m 644 /dev/null '/var/lib/juju/nonce.txt'",
			"printf '%s\\n' 'user-admin:bootstrap' > '/var/lib/juju/nonce.txt'",
		},
	})

	// check that a new instance will be started with a machine agent
	inst1, hc := testing.AssertStartInstance(c, env, "1")
	c.Check(*hc.Arch, gc.Equals, "amd64")
	c.Check(*hc.Mem, gc.Equals, uint64(1740))
	c.Check(*hc.CpuCores, gc.Equals, uint64(1))
	c.Assert(*hc.CpuPower, gc.Equals, uint64(100))
	inst = t.srv.ec2srv.Instance(string(inst1.Id()))
	c.Assert(inst, gc.NotNil)
	userData, err = utils.Gunzip(inst.UserData)
	c.Assert(err, gc.IsNil)
	c.Logf("second instance: UserData: %q", userData)
	userDataMap = nil
	err = goyaml.Unmarshal(userData, &userDataMap)
	c.Assert(err, gc.IsNil)
	CheckPackage(c, userDataMap, "curl", true)
	CheckPackage(c, userDataMap, "mongodb-server", false)
	CheckScripts(c, userDataMap, "jujud bootstrap-state", false)
	CheckScripts(c, userDataMap, "/var/lib/juju/agents/machine-1/agent.conf", true)
	// TODO check for provisioning agent

	err = env.Destroy()
	c.Assert(err, gc.IsNil)

	_, err = env.StateServerInstances()
	c.Assert(err, gc.Equals, environs.ErrNotBootstrapped)
}

// splitAuthKeys splits the given authorized keys
// into the form expected to be found in the
// user data.
func splitAuthKeys(keys string) []interface{} {
	slines := strings.FieldsFunc(keys, func(r rune) bool {
		return r == '\n'
	})
	var lines []interface{}
	for _, line := range slines {
		lines = append(lines, ssh.EnsureJujuComment(strings.TrimSpace(line)))
	}
	return lines
}

func (t *localServerSuite) TestInstanceStatus(c *gc.C) {
	env := t.Prepare(c)
	envtesting.UploadFakeTools(c, env.Storage())
	err := bootstrap.Bootstrap(coretesting.Context(c), env, environs.BootstrapParams{})
	c.Assert(err, gc.IsNil)
	t.srv.ec2srv.SetInitialInstanceState(ec2test.Terminated)
	inst, _ := testing.AssertStartInstance(c, env, "1")
	c.Assert(err, gc.IsNil)
	c.Assert(inst.Status(), gc.Equals, "terminated")
}

func (t *localServerSuite) TestStartInstanceHardwareCharacteristics(c *gc.C) {
	env := t.Prepare(c)
	envtesting.UploadFakeTools(c, env.Storage())
	err := bootstrap.Bootstrap(coretesting.Context(c), env, environs.BootstrapParams{})
	c.Assert(err, gc.IsNil)
	_, hc := testing.AssertStartInstance(c, env, "1")
	c.Check(*hc.Arch, gc.Equals, "amd64")
	c.Check(*hc.Mem, gc.Equals, uint64(1740))
	c.Check(*hc.CpuCores, gc.Equals, uint64(1))
	c.Assert(*hc.CpuPower, gc.Equals, uint64(100))
}

func (t *localServerSuite) TestStartInstanceAvailZone(c *gc.C) {
	inst, err := t.testStartInstanceAvailZone(c, "test-available")
	c.Assert(err, gc.IsNil)
	c.Assert(ec2.InstanceEC2(inst).AvailZone, gc.Equals, "test-available")
}

func (t *localServerSuite) TestStartInstanceAvailZoneImpaired(c *gc.C) {
	_, err := t.testStartInstanceAvailZone(c, "test-impaired")
	c.Assert(err, gc.ErrorMatches, `availability zone "test-impaired" is impaired`)
}

func (t *localServerSuite) TestStartInstanceAvailZoneUnknown(c *gc.C) {
	_, err := t.testStartInstanceAvailZone(c, "test-unknown")
	c.Assert(err, gc.ErrorMatches, `invalid availability zone "test-unknown"`)
}

func (t *localServerSuite) testStartInstanceAvailZone(c *gc.C, zone string) (instance.Instance, error) {
	env := t.Prepare(c)
	envtesting.UploadFakeTools(c, env.Storage())
	err := bootstrap.Bootstrap(coretesting.Context(c), env, environs.BootstrapParams{})
	c.Assert(err, gc.IsNil)

	params := environs.StartInstanceParams{Placement: "zone=" + zone}
	inst, _, _, err := testing.StartInstanceWithParams(env, "1", params, nil)
	return inst, err
}

func (t *localServerSuite) TestGetAvailabilityZones(c *gc.C) {
	var resultZones []amzec2.AvailabilityZoneInfo
	var resultErr error
	t.PatchValue(ec2.EC2AvailabilityZones, func(e *amzec2.EC2, f *amzec2.Filter) (*amzec2.AvailabilityZonesResp, error) {
		resp := &amzec2.AvailabilityZonesResp{
			Zones: append([]amzec2.AvailabilityZoneInfo{}, resultZones...),
		}
		return resp, resultErr
	})
	env := t.Prepare(c).(common.ZonedEnviron)

	resultErr = fmt.Errorf("failed to get availability zones")
	zones, err := env.AvailabilityZones()
	c.Assert(err, gc.Equals, resultErr)
	c.Assert(zones, gc.IsNil)

	resultErr = nil
	resultZones = make([]amzec2.AvailabilityZoneInfo, 1)
	resultZones[0].Name = "whatever"
	zones, err = env.AvailabilityZones()
	c.Assert(err, gc.IsNil)
	c.Assert(zones, gc.HasLen, 1)
	c.Assert(zones[0].Name(), gc.Equals, "whatever")

	// A successful result is cached, currently for the lifetime
	// of the Environ. This will change if/when we have long-lived
	// Environs to cut down repeated IaaS requests.
	resultErr = fmt.Errorf("failed to get availability zones")
	resultZones[0].Name = "andever"
	zones, err = env.AvailabilityZones()
	c.Assert(err, gc.IsNil)
	c.Assert(zones, gc.HasLen, 1)
	c.Assert(zones[0].Name(), gc.Equals, "whatever")
}

func (t *localServerSuite) TestGetAvailabilityZonesCommon(c *gc.C) {
	var resultZones []amzec2.AvailabilityZoneInfo
	t.PatchValue(ec2.EC2AvailabilityZones, func(e *amzec2.EC2, f *amzec2.Filter) (*amzec2.AvailabilityZonesResp, error) {
		resp := &amzec2.AvailabilityZonesResp{
			Zones: append([]amzec2.AvailabilityZoneInfo{}, resultZones...),
		}
		return resp, nil
	})
	env := t.Prepare(c).(common.ZonedEnviron)
	resultZones = make([]amzec2.AvailabilityZoneInfo, 2)
	resultZones[0].Name = "az1"
	resultZones[1].Name = "az2"
	resultZones[0].State = "available"
	resultZones[1].State = "impaired"
	zones, err := env.AvailabilityZones()
	c.Assert(err, gc.IsNil)
	c.Assert(zones, gc.HasLen, 2)
	c.Assert(zones[0].Name(), gc.Equals, resultZones[0].Name)
	c.Assert(zones[1].Name(), gc.Equals, resultZones[1].Name)
	c.Assert(zones[0].Available(), jc.IsTrue)
	c.Assert(zones[1].Available(), jc.IsFalse)
}

type mockAvailabilityZoneAllocations struct {
	group  []instance.Id // input param
	result []common.AvailabilityZoneInstances
	err    error
}

func (t *mockAvailabilityZoneAllocations) AvailabilityZoneAllocations(
	e common.ZonedEnviron, group []instance.Id,
) ([]common.AvailabilityZoneInstances, error) {
	t.group = group
	return t.result, t.err
}

func (t *localServerSuite) TestStartInstanceDistributionParams(c *gc.C) {
	env := t.Prepare(c)
	envtesting.UploadFakeTools(c, env.Storage())
	err := bootstrap.Bootstrap(coretesting.Context(c), env, environs.BootstrapParams{})
	c.Assert(err, gc.IsNil)

	mock := mockAvailabilityZoneAllocations{
		result: []common.AvailabilityZoneInstances{{ZoneName: "az1"}},
	}
	t.PatchValue(ec2.AvailabilityZoneAllocations, mock.AvailabilityZoneAllocations)

	// no distribution group specified
	testing.AssertStartInstance(c, env, "1")
	c.Assert(mock.group, gc.HasLen, 0)

	// distribution group specified: ensure it's passed through to AvailabilityZone.
	expectedInstances := []instance.Id{"i-0", "i-1"}
	params := environs.StartInstanceParams{
		DistributionGroup: func() ([]instance.Id, error) {
			return expectedInstances, nil
		},
	}
	_, _, _, err = testing.StartInstanceWithParams(env, "1", params, nil)
	c.Assert(err, gc.IsNil)
	c.Assert(mock.group, gc.DeepEquals, expectedInstances)
}

func (t *localServerSuite) TestStartInstanceDistributionErrors(c *gc.C) {
	env := t.Prepare(c)
	envtesting.UploadFakeTools(c, env.Storage())
	err := bootstrap.Bootstrap(coretesting.Context(c), env, environs.BootstrapParams{})
	c.Assert(err, gc.IsNil)

	mock := mockAvailabilityZoneAllocations{
		err: fmt.Errorf("AvailabilityZoneAllocations failed"),
	}
	t.PatchValue(ec2.AvailabilityZoneAllocations, mock.AvailabilityZoneAllocations)
	_, _, _, err = testing.StartInstance(env, "1")
	c.Assert(err, gc.Equals, mock.err)

	mock.err = nil
	dgErr := fmt.Errorf("DistributionGroup failed")
	params := environs.StartInstanceParams{
		DistributionGroup: func() ([]instance.Id, error) {
			return nil, dgErr
		},
	}
	_, _, _, err = testing.StartInstanceWithParams(env, "1", params, nil)
	c.Assert(err, gc.Equals, dgErr)
}

func (t *localServerSuite) TestStartInstanceDistribution(c *gc.C) {
	env := t.Prepare(c)
	envtesting.UploadFakeTools(c, env.Storage())
	err := bootstrap.Bootstrap(coretesting.Context(c), env, environs.BootstrapParams{})
	c.Assert(err, gc.IsNil)

	// test-available is the only available AZ, so AvailabilityZoneAllocations
	// is guaranteed to return that.
	inst, _ := testing.AssertStartInstance(c, env, "1")
	c.Assert(ec2.InstanceEC2(inst).AvailZone, gc.Equals, "test-available")
}

var azConstrainedErr = &amzec2.Error{
	Code:    "Unsupported",
	Message: "The requested Availability Zone is currently constrained etc.",
}

func (t *localServerSuite) TestStartInstanceAvailZoneAllConstrained(c *gc.C) {
	env := t.Prepare(c)
	envtesting.UploadFakeTools(c, env.Storage())
	err := bootstrap.Bootstrap(coretesting.Context(c), env, environs.BootstrapParams{})
	c.Assert(err, gc.IsNil)

	mock := mockAvailabilityZoneAllocations{
		result: []common.AvailabilityZoneInstances{
			{ZoneName: "az1"}, {ZoneName: "az2"},
		},
	}
	t.PatchValue(ec2.AvailabilityZoneAllocations, mock.AvailabilityZoneAllocations)

	var azArgs []string
	t.PatchValue(ec2.RunInstances, func(e *amzec2.EC2, ri *amzec2.RunInstances) (*amzec2.RunInstancesResp, error) {
		azArgs = append(azArgs, ri.AvailZone)
		return nil, azConstrainedErr
	})
	_, _, _, err = testing.StartInstance(env, "1")
	c.Assert(err, gc.ErrorMatches, `cannot run instances: The requested Availability Zone is currently constrained etc\. \(Unsupported\)`)
	c.Assert(azArgs, gc.DeepEquals, []string{"az1", "az2"})
}

func (t *localServerSuite) TestStartInstanceAvailZoneOneConstrained(c *gc.C) {
	env := t.Prepare(c)
	envtesting.UploadFakeTools(c, env.Storage())
	err := bootstrap.Bootstrap(coretesting.Context(c), env, environs.BootstrapParams{})
	c.Assert(err, gc.IsNil)

	mock := mockAvailabilityZoneAllocations{
		result: []common.AvailabilityZoneInstances{
			{ZoneName: "az1"}, {ZoneName: "az2"},
		},
	}
	t.PatchValue(ec2.AvailabilityZoneAllocations, mock.AvailabilityZoneAllocations)

	// The first call to RunInstances fails with an error indicating the AZ
	// is constrained. The second attempt succeeds, and so allocates to az2.
	var azArgs []string
	realRunInstances := *ec2.RunInstances
	t.PatchValue(ec2.RunInstances, func(e *amzec2.EC2, ri *amzec2.RunInstances) (*amzec2.RunInstancesResp, error) {
		azArgs = append(azArgs, ri.AvailZone)
		if len(azArgs) == 1 {
			return nil, azConstrainedErr
		}
		return realRunInstances(e, ri)
	})
	inst, _ := testing.AssertStartInstance(c, env, "1")
	c.Assert(azArgs, gc.DeepEquals, []string{"az1", "az2"})
	c.Assert(ec2.InstanceEC2(inst).AvailZone, gc.Equals, "az2")
}

func (t *localServerSuite) TestAddresses(c *gc.C) {
	env := t.Prepare(c)
	envtesting.UploadFakeTools(c, env.Storage())
	err := bootstrap.Bootstrap(coretesting.Context(c), env, environs.BootstrapParams{})
	c.Assert(err, gc.IsNil)
	inst, _ := testing.AssertStartInstance(c, env, "1")
	c.Assert(err, gc.IsNil)
	addrs, err := inst.Addresses()
	c.Assert(err, gc.IsNil)
	// Expected values use Address type but really contain a regexp for
	// the value rather than a valid ip or hostname.
	expected := []network.Address{{
		Value: "*.testing.invalid",
		Type:  network.HostName,
		Scope: network.ScopePublic,
	}, {
		Value: "*.internal.invalid",
		Type:  network.HostName,
		Scope: network.ScopeCloudLocal,
	}, {
		Value: "8.0.0.*",
		Type:  network.IPv4Address,
		Scope: network.ScopePublic,
	}, {
		Value: "127.0.0.*",
		Type:  network.IPv4Address,
		Scope: network.ScopeCloudLocal,
	}}
	c.Assert(addrs, gc.HasLen, len(expected))
	for i, addr := range addrs {
		c.Check(addr.Value, gc.Matches, expected[i].Value)
		c.Check(addr.Type, gc.Equals, expected[i].Type)
		c.Check(addr.Scope, gc.Equals, expected[i].Scope)
	}
}

func (t *localServerSuite) TestConstraintsValidatorUnsupported(c *gc.C) {
	env := t.Prepare(c)
	validator, err := env.ConstraintsValidator()
	c.Assert(err, gc.IsNil)
	cons := constraints.MustParse("arch=amd64 tags=foo")
	unsupported, err := validator.Validate(cons)
	c.Assert(err, gc.IsNil)
	c.Assert(unsupported, gc.DeepEquals, []string{"tags"})
}

func (t *localServerSuite) TestConstraintsValidatorVocab(c *gc.C) {
	env := t.Prepare(c)
	validator, err := env.ConstraintsValidator()
	c.Assert(err, gc.IsNil)
	cons := constraints.MustParse("arch=ppc64el")
	_, err = validator.Validate(cons)
	c.Assert(err, gc.ErrorMatches, "invalid constraint value: arch=ppc64el\nvalid values are:.*")
	cons = constraints.MustParse("instance-type=foo")
	_, err = validator.Validate(cons)
	c.Assert(err, gc.ErrorMatches, "invalid constraint value: instance-type=foo\nvalid values are:.*")
}

func (t *localServerSuite) TestConstraintsMerge(c *gc.C) {
	env := t.Prepare(c)
	validator, err := env.ConstraintsValidator()
	c.Assert(err, gc.IsNil)
	consA := constraints.MustParse("arch=amd64 mem=1G cpu-power=10 cpu-cores=2 tags=bar")
	consB := constraints.MustParse("arch=i386 instance-type=m1.small")
	cons, err := validator.Merge(consA, consB)
	c.Assert(err, gc.IsNil)
	c.Assert(cons, gc.DeepEquals, constraints.MustParse("arch=i386 instance-type=m1.small tags=bar"))
}

func (t *localServerSuite) TestPrecheckInstanceValidInstanceType(c *gc.C) {
	env := t.Prepare(c)
	cons := constraints.MustParse("instance-type=m1.small root-disk=1G")
	placement := ""
	err := env.PrecheckInstance("precise", cons, placement)
	c.Assert(err, gc.IsNil)
}

func (t *localServerSuite) TestPrecheckInstanceInvalidInstanceType(c *gc.C) {
	env := t.Prepare(c)
	cons := constraints.MustParse("instance-type=m1.invalid")
	placement := ""
	err := env.PrecheckInstance("precise", cons, placement)
	c.Assert(err, gc.ErrorMatches, `invalid AWS instance type "m1.invalid" specified`)
}

func (t *localServerSuite) TestPrecheckInstanceUnsupportedArch(c *gc.C) {
	env := t.Prepare(c)
	cons := constraints.MustParse("instance-type=cc1.4xlarge arch=i386")
	placement := ""
	err := env.PrecheckInstance("precise", cons, placement)
	c.Assert(err, gc.ErrorMatches, `invalid AWS instance type "cc1.4xlarge" and arch "i386" specified`)
}

func (t *localServerSuite) TestPrecheckInstanceAvailZone(c *gc.C) {
	env := t.Prepare(c)
	placement := "zone=test-available"
	err := env.PrecheckInstance("precise", constraints.Value{}, placement)
	c.Assert(err, gc.IsNil)
}

func (t *localServerSuite) TestPrecheckInstanceAvailZoneUnavailable(c *gc.C) {
	env := t.Prepare(c)
	placement := "zone=test-unavailable"
	err := env.PrecheckInstance("precise", constraints.Value{}, placement)
	c.Assert(err, gc.IsNil)
}

func (t *localServerSuite) TestPrecheckInstanceAvailZoneUnknown(c *gc.C) {
	env := t.Prepare(c)
	placement := "zone=test-unknown"
	err := env.PrecheckInstance("precise", constraints.Value{}, placement)
	c.Assert(err, gc.ErrorMatches, `invalid availability zone "test-unknown"`)
}

func (t *localServerSuite) TestValidateImageMetadata(c *gc.C) {
	env := t.Prepare(c)
	params, err := env.(simplestreams.MetadataValidator).MetadataLookupParams("test")
	c.Assert(err, gc.IsNil)
	params.Series = "precise"
	params.Endpoint = "https://ec2.endpoint.com"
	params.Sources, err = imagemetadata.GetMetadataSources(env)
	c.Assert(err, gc.IsNil)
	image_ids, _, err := imagemetadata.ValidateImageMetadata(params)
	c.Assert(err, gc.IsNil)
	sort.Strings(image_ids)
	c.Assert(image_ids, gc.DeepEquals, []string{"ami-00000033", "ami-00000034", "ami-00000035"})
}

func (t *localServerSuite) TestGetToolsMetadataSources(c *gc.C) {
	env := t.Prepare(c)
	sources, err := tools.GetMetadataSources(env)
	c.Assert(err, gc.IsNil)
	c.Assert(len(sources), gc.Equals, 1)
	url, err := sources[0].URL("")
	// The control bucket URL contains the bucket name.
	c.Assert(strings.Contains(url, ec2.ControlBucketName(env)+"/tools"), jc.IsTrue)
}

func (t *localServerSuite) TestSupportedArchitectures(c *gc.C) {
	env := t.Prepare(c)
	a, err := env.SupportedArchitectures()
	c.Assert(err, gc.IsNil)
	c.Assert(a, jc.SameContents, []string{"amd64", "i386"})
}

func (t *localServerSuite) TestSupportNetworks(c *gc.C) {
	env := t.Prepare(c)
	c.Assert(env.SupportNetworks(), jc.IsFalse)
}

// localNonUSEastSuite is similar to localServerSuite but the S3 mock server
// behaves as if it is not in the us-east region.
type localNonUSEastSuite struct {
	coretesting.BaseSuite
	restoreEC2Patching func()
	srv                localServer
	env                environs.Environ
}

func (t *localNonUSEastSuite) SetUpSuite(c *gc.C) {
	t.BaseSuite.SetUpSuite(c)
	t.restoreEC2Patching = patchEC2ForTesting()
}

func (t *localNonUSEastSuite) TearDownSuite(c *gc.C) {
	t.restoreEC2Patching()
	t.BaseSuite.TearDownSuite(c)
}

func (t *localNonUSEastSuite) SetUpTest(c *gc.C) {
	t.BaseSuite.SetUpTest(c)
	t.srv.config = &s3test.Config{
		Send409Conflict: true,
	}
	t.srv.startServer(c)

	cfg, err := config.New(config.NoDefaults, localConfigAttrs)
	c.Assert(err, gc.IsNil)
	env, err := environs.Prepare(cfg, coretesting.Context(c), configstore.NewMem())
	c.Assert(err, gc.IsNil)
	t.env = env
}

func (t *localNonUSEastSuite) TearDownTest(c *gc.C) {
	t.srv.stopServer(c)
	t.BaseSuite.TearDownTest(c)
}

func patchEC2ForTesting() func() {
	ec2.UseTestImageData(ec2.TestImagesData)
	ec2.UseTestInstanceTypeData(ec2.TestInstanceTypeCosts)
	ec2.UseTestRegionData(ec2.TestRegions)
	restoreTimeouts := envtesting.PatchAttemptStrategies(ec2.ShortAttempt, ec2.StorageAttempt)
	restoreFinishBootstrap := envtesting.DisableFinishBootstrap()
	return func() {
		restoreFinishBootstrap()
		restoreTimeouts()
		ec2.UseTestImageData(nil)
		ec2.UseTestInstanceTypeData(nil)
		ec2.UseTestRegionData(nil)
	}
}

// If match is true, CheckScripts checks that at least one script started
// by the cloudinit data matches the given regexp pattern, otherwise it
// checks that no script matches.  It's exported so it can be used by tests
// defined in ec2_test.
func CheckScripts(c *gc.C, userDataMap map[interface{}]interface{}, pattern string, match bool) {
	scripts0 := userDataMap["runcmd"]
	if scripts0 == nil {
		c.Errorf("cloudinit has no entry for runcmd")
		return
	}
	scripts := scripts0.([]interface{})
	re := regexp.MustCompile(pattern)
	found := false
	for _, s0 := range scripts {
		s := s0.(string)
		if re.MatchString(s) {
			found = true
		}
	}
	switch {
	case match && !found:
		c.Errorf("script %q not found in %q", pattern, scripts)
	case !match && found:
		c.Errorf("script %q found but not expected in %q", pattern, scripts)
	}
}

// CheckPackage checks that the cloudinit will or won't install the given
// package, depending on the value of match.  It's exported so it can be
// used by tests defined outside the ec2 package.
func CheckPackage(c *gc.C, userDataMap map[interface{}]interface{}, pkg string, match bool) {
	pkgs0 := userDataMap["packages"]
	if pkgs0 == nil {
		if match {
			c.Errorf("cloudinit has no entry for packages")
		}
		return
	}

	pkgs := pkgs0.([]interface{})

	found := false
	for _, p0 := range pkgs {
		p := p0.(string)
		if p == pkg {
			found = true
		}
	}
	switch {
	case match && !found:
		c.Errorf("package %q not found in %v", pkg, pkgs)
	case !match && found:
		c.Errorf("%q found but not expected in %v", pkg, pkgs)
	}
}
