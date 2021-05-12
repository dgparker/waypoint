package serverinstall

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/aws/aws-sdk-go/service/efs"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/resourcegroups"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/waypoint-plugin-sdk/component"
	"github.com/hashicorp/waypoint-plugin-sdk/terminal"
	"github.com/hashicorp/waypoint/builtin/aws/utils"
	"github.com/hashicorp/waypoint/internal/clicontext"
	"github.com/hashicorp/waypoint/internal/pkg/flag"
	pb "github.com/hashicorp/waypoint/internal/server/gen"
	"github.com/hashicorp/waypoint/internal/serverconfig"
	"github.com/ryboe/q"
)

type ECSInstaller struct {
	config ecsConfig
}

type ecsConfig struct {
	ServerImage string `hcl:"server_image,optional"`

	AdvertiseInternal bool   `hcl:"advertise_internal,optional"`
	ImagePullPolicy   string `hcl:"image_pull_policy,optional"`
	// How much CPU to assign to the containers
	CPU string `hcl:"cpu,optional"`
	// How much memory to assign to the containers
	Memory string `hcl:"memory"`
	// The soft limit (in MiB) of memory to reserve for the container
	MemoryReservation string `hcl:"memory_reservation,optional"`

	Region string `hcl:"region,optional"`

	// Name of the ECS cluster to install the service into
	Cluster string `hcl:"cluster,optional"`

	// Port that your service is running on within the actual container.
	// Defaults to port 3000.
	ServicePort int64 `hcl:"service_port,optional"`

	// Indicate that service should be deployed on an EC2 cluster.
	EC2Cluster bool `hcl:"ec2_cluster,optional"`

	// Name of the execution task IAM Role to associate with the ECS Service
	ExecutionRoleName string `hcl:"execution_role_name,optional"`

	// Subnets to place the service into. Defaults to the subnets in the default VPC.
	Subnets []string `hcl:"subnets,optional"`
}

// Install is a method of ECSInstaller and implements the Installer interface to
// register a waypoint-server in a ecs cluster
func (i *ECSInstaller) Install(
	ctx context.Context,
	opts *InstallOpts,
) (*InstallResults, error) {
	ui := opts.UI
	log := opts.Log

	sg := ui.StepGroup()
	defer sg.Wait()

	s := sg.Add("Inspecting ECS cluster...")
	defer func() { s.Abort() }()
	log.Info("Starting lifecycle")

	var (
		sess *session.Session

		executionRole, taskRole, cluster, serverLogGroup string

		dep *ecsServer

		efsId string

		err error
	)

	start := time.Now()
	defer func() {
		q.Q("->Total time:", time.Since(start).Minutes())
	}()

	// TODO: config this
	taskRole = "arn:aws:iam::797645259670:role/waypoint-ecs-task-role"

	ulid, err := component.Id()
	if err != nil {
		return nil, err
	}

	serverLogGroup = "waypoint-server-logs"
	// TODO: reuse this for runner setup
	lf := &Lifecycle{
		Init: func(s LifecycleStatus) error {
			sess, err = utils.GetSession(&utils.SessionConfig{
				Region: i.config.Region,
				Logger: log,
			})
			if err != nil {
				return err
			}
			cluster, err = i.SetupCluster(ctx, s, sess, ulid)
			if err != nil {
				return err
			}

			efsId, err = i.SetupEFS(ctx, s, sess, ulid)
			if err != nil {
				return err
			}

			executionRole, err = i.SetupExecutionRole(ctx, s, log, sess, ulid)
			if err != nil {
				return err
			}

			serverLogGroup, err = i.SetupLogs(ctx, s, log, sess, ulid, serverLogGroup)
			if err != nil {
				return err
			}

			return nil
		},

		Run: func(s LifecycleStatus) error {
			dep, err = i.Launch(ctx, s, log, ui, sess, efsId, executionRole, taskRole, cluster, serverLogGroup, ulid)
			return err
		},

		Cleanup: func(s LifecycleStatus) error { return nil },
	}

	if err := lf.Execute(log, ui); err != nil {
		return nil, err
	}

	// Set our connection information
	var contextConfig clicontext.Config
	var advertiseAddr pb.ServerConfig_AdvertiseAddr
	var httpAddr string
	var grpcAddr string
	grpcAddr = fmt.Sprintf("%s:%s", dep.Url, grpcPort)
	httpAddr = fmt.Sprintf("%s:%s", dep.Url, httpPort)
	// Set our advertise address
	advertiseAddr.Addr = grpcAddr
	advertiseAddr.Tls = true
	advertiseAddr.TlsSkipVerify = true
	contextConfig = clicontext.Config{
		Server: serverconfig.Client{
			Address:       grpcAddr,
			Tls:           true,
			TlsSkipVerify: true, // always for now
			Platform:      "ecs",
		},
	}
	s.Done()
	return &InstallResults{
		Context:       &contextConfig,
		AdvertiseAddr: &advertiseAddr,
		HTTPAddr:      httpAddr,
	}, nil
}

// Upgrade is a method of ECSInstaller and implements the Installer interface to
// upgrade a waypoint-server in a ecs cluster
func (i *ECSInstaller) Upgrade(
	ctx context.Context, opts *InstallOpts, serverCfg serverconfig.Client) (
	*InstallResults, error,
) {
	ui := opts.UI
	log := opts.Log
	sess, err := utils.GetSession(&utils.SessionConfig{
		Region: i.config.Region,
		Logger: log,
	})
	if err != nil {
		return nil, err
	}

	sg := ui.StepGroup()
	defer sg.Wait()

	s := sg.Add("Inspecting ecs cluster...")
	defer s.Abort()

	// inspect current service - looking for image used in Task
	// Get Task definition
	var clusterArn string
	cluster := i.config.Cluster
	// shouldn't need this though
	if cluster == "" {
		cluster = "waypoint-server"
	}
	ecsSvc := ecs.New(sess)

	desc, err := ecsSvc.DescribeClusters(&ecs.DescribeClustersInput{
		Clusters: []*string{aws.String(cluster)},
	})
	if err != nil {
		return nil, err
	}

	var found bool
	for _, c := range desc.Clusters {
		if *c.ClusterName == cluster && strings.ToLower(*c.Status) == "active" {
			clusterArn = *c.ClusterArn
			found = true
			s.Update("Found existing ECS cluster: %s", cluster)
		}
	}
	if !found {
		return nil, fmt.Errorf("error: could not find ecs cluster")
	}
	// list the services to find the task descriptions
	services, err := ecsSvc.DescribeServices(&ecs.DescribeServicesInput{
		Cluster:  aws.String("waypoint-server"),
		Services: []*string{aws.String(serverName)},
	})
	if err != nil {
		return nil, err
	}
	// should only find one
	serverSvc := services.Services[0]
	if serverSvc == nil {
		return nil, fmt.Errorf("no waypoint-server service found")
	}

	def, err := ecsSvc.DescribeTaskDefinition(&ecs.DescribeTaskDefinitionInput{
		Include:        []*string{aws.String("TAGS")},
		TaskDefinition: serverSvc.TaskDefinition,
	})
	if err != nil {
		return nil, err
	}

	// assume only 1 task running here
	taskDef := def.TaskDefinition
	taskTags := def.Tags
	containerDef := taskDef.ContainerDefinitions[0]

	upgradeImg := defaultServerImage
	if i.config.ServerImage != "" {
		upgradeImg = i.config.ServerImage
	}
	// assume upgrade to latest
	if *containerDef.Image == defaultServerImage {
		// we can just update/force-deploy the service
		_, err := ecsSvc.UpdateService(&ecs.UpdateServiceInput{
			ForceNewDeployment:            aws.Bool(true),
			Cluster:                       &clusterArn,
			Service:                       serverSvc.ServiceName,
			HealthCheckGracePeriodSeconds: aws.Int64(int64(600)),
		})
		if err != nil {
			return nil, err
		}
		err = ecsSvc.WaitUntilServicesStable(&ecs.DescribeServicesInput{
			Cluster:  &clusterArn,
			Services: []*string{serverSvc.ServiceName},
		})
	} else {
		containerDef.Image = &upgradeImg
		// update task definition

		taskDef.SetContainerDefinitions([]*ecs.ContainerDefinition{containerDef})
		runtime := aws.String("FARGATE")
		registerTaskDefinitionInput := ecs.RegisterTaskDefinitionInput{
			ContainerDefinitions: taskDef.ContainerDefinitions,

			// TODO: execustion role for Runner
			ExecutionRoleArn: taskDef.ExecutionRoleArn,
			Cpu:              taskDef.Cpu,
			Memory:           taskDef.Memory,
			Family:           taskDef.Family,

			NetworkMode:             aws.String("awsvpc"),
			RequiresCompatibilities: []*string{runtime},
			Tags:                    taskTags,
			// TODO: configurable / create EFS
			Volumes: taskDef.Volumes,
		}

		var taskOut *ecs.RegisterTaskDefinitionOutput
		ecsSvc := ecs.New(sess)
		// AWS is eventually consistent so even though we probably created the
		// resources that are referenced by the task definition, it can error out if
		// we try to reference those resources too quickly. So we're forced to guard
		// actions which reference other AWS services with loops like this.
		for i := 0; i < 30; i++ {
			taskOut, err = ecsSvc.RegisterTaskDefinition(&registerTaskDefinitionInput)
			if err == nil {
				break
			}

			// if we encounter an unrecoverable error, exit now.
			if aerr, ok := err.(awserr.Error); ok {
				switch aerr.Code() {
				case "ResourceConflictException":
					return nil, err
				}
			}

			// otherwise sleep and try again
			time.Sleep(2 * time.Second)
		}

		if err != nil {
			return nil, err
		}

		_, err := ecsSvc.UpdateService(&ecs.UpdateServiceInput{
			Cluster:        &clusterArn,
			TaskDefinition: taskOut.TaskDefinition.TaskDefinitionArn,
			Service:        serverSvc.ServiceName,
		})
		if err != nil {
			return nil, err
		}
		err = ecsSvc.WaitUntilServicesStable(&ecs.DescribeServicesInput{
			Cluster:  &clusterArn,
			Services: []*string{serverSvc.ServiceName},
		})
	}

	s.Done()
	var contextConfig clicontext.Config
	var advertiseAddr pb.ServerConfig_AdvertiseAddr
	advertiseAddr.Addr = serverCfg.Address
	advertiseAddr.Tls = true
	advertiseAddr.TlsSkipVerify = true
	contextConfig = clicontext.Config{
		Server: serverCfg,
	}
	httpAddr := strings.Replace(serverCfg.Address, "9701", "9702", 1)

	s.Done()
	return &InstallResults{
		Context:       &contextConfig,
		AdvertiseAddr: &advertiseAddr,
		HTTPAddr:      httpAddr,
	}, nil
}

// Uninstall is a method of ECSInstaller and implements the Installer interface
// to remove a waypoint-server statefulset and the associated PVC and service
// from a ecs cluster
func (i *ECSInstaller) Uninstall(ctx context.Context, opts *InstallOpts) error {
	ui := opts.UI
	log := opts.Log

	s := new(lStatus)
	s.ui = ui
	defer s.Close()

	s.Status("Examining Resource Group for ECS Server...")
	// Get list of things
	sess, err := utils.GetSession(&utils.SessionConfig{
		Region: i.config.Region,
		Logger: log,
	})
	if err != nil {
		return err
	}
	rgSvc := resourcegroups.New(sess)

	serverQuery := "{\"ResourceTypeFilters\":[\"AWS::AllSupported\"],\"TagFilters\":[{\"Key\":\"waypoint-server\",\"Values\":[]}]}"
	runnerQuery := "{\"ResourceTypeFilters\":[\"AWS::AllSupported\"],\"TagFilters\":[{\"Key\":\"waypoint-runner\",\"Values\":[]}]}"

	sg, err := rgSvc.SearchResources(&resourcegroups.SearchResourcesInput{
		ResourceQuery: &resourcegroups.ResourceQuery{
			Type:  aws.String(resourcegroups.QueryTypeTagFilters10),
			Query: aws.String(serverQuery),
		},
	})
	if err != nil {
		return err
	}
	rg, err := rgSvc.SearchResources(&resourcegroups.SearchResourcesInput{
		ResourceQuery: &resourcegroups.ResourceQuery{
			Type:  aws.String(resourcegroups.QueryTypeTagFilters10),
			Query: aws.String(runnerQuery),
		},
	})
	if err != nil {
		return err
	}
	// Start destroying things. Some cannot be destroyed before others. The
	// general order to destroy things:
	// - ECS Service
	// - ECS Cluster
	// - Cloudwatch Log Group
	// - ELB Target Groups
	// - ELB Network Load Balancer
	// - EFS File System

	resources := append(sg.ResourceIdentifiers, rg.ResourceIdentifiers...)

	s.Status("Deleting ECS resources...")
	if err := deleteEcsResources(ctx, s, sess, resources); err != nil {
		return err
	}
	s.Status("Deleting Cloud Watch Log Group resources...")
	if err := deleteCWLResources(ctx, s, sess, resources); err != nil {
		return err
	}
	s.Status("Deleting EFS resources...")
	if err := deleteEFSResources(ctx, s, sess, resources); err != nil {
		return err
	}
	s.Status("Deleting Network resources...")
	if err := deleteNLBResources(ctx, s, sess, resources); err != nil {
		return err
	}

	s.Status("Resources deleted")
	return nil
}

func deleteEFSResources(
	ctx context.Context,
	s LifecycleStatus,
	sess *session.Session,
	resources []*resourcegroups.ResourceIdentifier,
) error {
	// 	"AWS::EFS::FileSystem",
	var id string
	for _, r := range resources {
		if *r.ResourceType == "AWS::EFS::FileSystem" {
			id = nameFromArn(*r.ResourceArn)
			break
		}
	}
	efsSvc := efs.New(sess)
	mtgs, err := efsSvc.DescribeMountTargets(&efs.DescribeMountTargetsInput{
		FileSystemId: &id,
	})
	if err != nil {
		return err
	}
	s.Update("Deleting EFS Mount Targets...")
	for _, mt := range mtgs.MountTargets {
		_, err := efsSvc.DeleteMountTarget(&efs.DeleteMountTargetInput{
			MountTargetId: mt.MountTargetId,
		})
		if err != nil {
			return err
		}
	}
	for i := 0; 1 < 30; i++ {
		mtgs, err := efsSvc.DescribeMountTargets(&efs.DescribeMountTargetsInput{
			FileSystemId: &id,
		})
		if err != nil {
			return err
		}

		var deleted int
		mtgCount := len(mtgs.MountTargets)

		for _, m := range mtgs.MountTargets {
			if *m.LifeCycleState == efs.LifeCycleStateDeleted {
				deleted++
			}
		}
		if mtgCount == 0 {
			break
		}

		if deleted == mtgCount {
		}

		time.Sleep(5 * time.Second)
		continue
	}

	s.Update("Deleting EFS File System...")
	_, err = efsSvc.DeleteFileSystem(&efs.DeleteFileSystemInput{
		FileSystemId: &id,
	})
	if err != nil {
		return err
	}
	return nil
}

func deleteNLBResources(
	ctx context.Context,
	s LifecycleStatus,
	sess *session.Session,
	resources []*resourcegroups.ResourceIdentifier,
) error {
	elbSvc := elbv2.New(sess)
	for _, r := range resources {
		if *r.ResourceType == "AWS::ElasticLoadBalancingV2::LoadBalancer" {
			s.Update("Deleting Network Load Balancer LISTENERS", *r.ResourceArn)
			results, err := elbSvc.DescribeListeners(&elbv2.DescribeListenersInput{
				LoadBalancerArn: r.ResourceArn,
			})
			if err != nil {
				return err
			}
			for _, l := range results.Listeners {
				_, err := elbSvc.DeleteListener(&elbv2.DeleteListenerInput{
					ListenerArn: l.ListenerArn,
				})
				if err != nil {
					return err
				}
			}
			// s.Update("Deleting Network Load Balancer ", *r.ResourceArn)
			// _, err := elbSvc.DeleteLoadBalancer(&elbv2.DeleteLoadBalancerInput{
			// 	LoadBalancerArn: r.ResourceArn,
			// })
			// if err != nil {
			// 	return err
			// }
		}
	}
	s.Update("Deleting Target Groups...")
	for _, r := range resources {
		if *r.ResourceType == "AWS::ElasticLoadBalancingV2::TargetGroup" {
			_, err := elbSvc.DeleteTargetGroup(&elbv2.DeleteTargetGroupInput{
				TargetGroupArn: r.ResourceArn,
			})
			if err != nil {
				return err
			}
		}
	}

	s.Update("Deleting Security Groups...")
	ec2Svc := ec2.New(sess)
	results, err := ec2Svc.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("tag-key"),
				Values: []*string{aws.String("waypoint-server")},
			},
		},
	})
	if err != nil {
		return err
	}
	if len(results.SecurityGroups) > 0 {
		for _, g := range results.SecurityGroups {
			for i := 0; i < 20; i++ {
				_, err := ec2Svc.DeleteSecurityGroup(&ec2.DeleteSecurityGroupInput{
					GroupId: g.GroupId,
				})
				// if we encounter an unrecoverable error, exit now.
				if aerr, ok := err.(awserr.Error); ok {
					switch aerr.Code() {
					case "DependencyViolation":
						time.Sleep(2 * time.Second)
						continue
					default:
						return err
					}
				}
				return err
			}
		}
	}

	return nil
}

func nameFromArn(arn string) string {
	parts := strings.Split(arn, ":")
	last := parts[len(parts)-1]
	parts = strings.Split(last, "/")
	return parts[len(parts)-1]
}

func deleteCWLResources(
	ctx context.Context,
	s LifecycleStatus,
	sess *session.Session,
	resources []*resourcegroups.ResourceIdentifier,
) error {
	cwlSvc := cloudwatchlogs.New(sess)
	groups, err := cwlSvc.DescribeLogGroups(&cloudwatchlogs.DescribeLogGroupsInput{
		LogGroupNamePrefix: aws.String("waypoint"),
	})
	if err != nil {
		return err
	}

	for _, l := range groups.LogGroups {
		s.Update("Deleting Log Group %s", *l.LogGroupName)
		_, err := cwlSvc.DeleteLogGroup(&cloudwatchlogs.DeleteLogGroupInput{
			LogGroupName: l.LogGroupName,
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func deleteEcsResources(
	ctx context.Context,
	s LifecycleStatus,
	sess *session.Session,
	resources []*resourcegroups.ResourceIdentifier,
) error {
	ecsSvc := ecs.New(sess)

	var clusterArn string
	for _, r := range resources {
		if *r.ResourceType == "AWS::ECS::Cluster" {
			clusterArn = *r.ResourceArn
		}
	}

	results, err := ecsSvc.ListServices(&ecs.ListServicesInput{
		Cluster: &clusterArn,
	})
	if err != nil {
		q.Q("-> -> describe services err:", err)
	}

	for _, service := range results.ServiceArns {
		s.Update("Deleting ECS service: %s", *service)
		_, err := ecsSvc.DeleteService(&ecs.DeleteServiceInput{
			Service: service,
			Force:   aws.Bool(true),
			Cluster: &clusterArn,
		})
		if err != nil {
			return err
		}
	}

	runningTasks, err := ecsSvc.ListTasks(&ecs.ListTasksInput{
		Cluster:       &clusterArn,
		DesiredStatus: aws.String(ecs.DesiredStatusRunning),
	})
	if err != nil {
		return err
	}

	for _, task := range runningTasks.TaskArns {
		_, err := ecsSvc.StopTask(&ecs.StopTaskInput{
			Cluster: &clusterArn,
			Task:    task,
		})
		if err != nil {
			return err
		}
	}

	err = ecsSvc.WaitUntilServicesInactive(&ecs.DescribeServicesInput{
		Cluster:  &clusterArn,
		Services: results.ServiceArns,
	})
	for _, r := range resources {
		if *r.ResourceType == "AWS::ECS::TaskDefinition" {
			s.Update("Deregistering ECS task:", *r.ResourceArn)
			_, err := ecsSvc.DeregisterTaskDefinition(&ecs.DeregisterTaskDefinitionInput{
				TaskDefinition: r.ResourceArn,
			})
			if err != nil {
				return err
			}
		}
	}

	s.Update("Deleting ECS cluster: %s", clusterArn)
	_, err = ecsSvc.DeleteCluster(&ecs.DeleteClusterInput{
		Cluster: &clusterArn,
	})
	if err != nil {
		return err
	}

	return nil
}

// InstallRunner implements Installer.
func (i *ECSInstaller) InstallRunner(
	ctx context.Context,
	opts *InstallRunnerOpts,
) error {
	ui := opts.UI
	log := opts.Log
	ulid, err := component.Id()
	if err != nil {
		return err
	}

	sg := ui.StepGroup()
	defer sg.Wait()
	sess, err := utils.GetSession(&utils.SessionConfig{
		Region: i.config.Region,
		Logger: log,
	})
	if err != nil {
		return err
	}

	s := sg.Add("Starting Runner installation")
	defer func() { s.Abort() }()
	// TODO: dynamic log group
	runnerLogGroup := "waypoint-runner-logs"
	var logGroup string
	// TODO: reuse this for runner setup
	lf := &Lifecycle{
		Init: func(s LifecycleStatus) error {
			sess, err = utils.GetSession(&utils.SessionConfig{
				Region: i.config.Region,
				Logger: log,
			})
			if err != nil {
				return err
			}

			logGroup, err = i.SetupLogs(ctx, s, log, sess, ulid, runnerLogGroup)
			if err != nil {
				return err
			}

			return nil
		},

		Run: func(s LifecycleStatus) error {
			return nil
		},

		Cleanup: func(s LifecycleStatus) error { return nil },
	}

	if err := lf.Execute(log, ui); err != nil {
		return err
	}

	defaultStreamPrefix := fmt.Sprintf("waypoint-runner-%d", time.Now().Nanosecond())
	logOptions := buildLoggingOptions(
		nil,
		i.config.Region,
		logGroup,
		defaultStreamPrefix,
	)
	var (
		defaultResourcesCPU    = 256
		defaultResourcesMemory = 512
	)
	// TODO convert these from config
	cpu := defaultResourcesCPU
	mem := defaultResourcesMemory
	grpcPort, _ := strconv.Atoi(defaultGrpcPort)

	cmd := []*string{
		aws.String("runner"),
		aws.String("agent"),
		aws.String("-vvv"),
		aws.String("-liveness-tcp-addr=:1234"),
	}

	envs := []*ecs.KeyValuePair{}
	for _, line := range opts.AdvertiseClient.Env() {
		idx := strings.Index(line, "=")
		if idx == -1 {
			// Should never happen but let's not crash.
			continue
		}

		key := line[:idx]
		value := line[idx+1:]
		envs = append(envs, &ecs.KeyValuePair{
			Name:  aws.String(key),
			Value: aws.String(value),
		})
	}

	def := ecs.ContainerDefinition{
		Essential: aws.Bool(true),
		Command:   cmd,
		Name:      aws.String("waypoint-runner"),
		Image:     aws.String(i.config.ServerImage),
		PortMappings: []*ecs.PortMapping{
			{
				// TODO: configurable port
				ContainerPort: aws.Int64(int64(grpcPort)),
			},
		},
		Environment: envs,
		// TODO: These are optional for Fargate
		Memory: utils.OptionalInt64(int64(mem)),
		LogConfiguration: &ecs.LogConfiguration{
			LogDriver: aws.String("awslogs"),
			Options:   logOptions,
		},
	}
	mems := strconv.Itoa(mem)
	if mem == 0 {
		return fmt.Errorf("Memory value required for Runner")
	}
	cpuValues, ok := fargateResources[mem]
	if !ok {
		var (
			allValues  []int
			goodValues []string
		)

		for k := range fargateResources {
			allValues = append(allValues, k)
		}

		sort.Ints(allValues)

		for _, k := range allValues {
			goodValues = append(goodValues, strconv.Itoa(k))
		}

		return fmt.Errorf("Invalid memory value: %d (valid values: %s)",
			mem, strings.Join(goodValues, ", "))
	}

	var cpuShares int
	if cpu == 0 {
		cpuShares = cpuValues[0]
	} else {
		var (
			valid      bool
			goodValues []string
		)

		for _, c := range cpuValues {
			goodValues = append(goodValues, strconv.Itoa(c))
			if c == cpu {
				valid = true
				break
			}
		}

		if !valid {
			return fmt.Errorf("Invalid cpu value: %d (valid values: %s)",
				mem, strings.Join(goodValues, ", "))
		}

		cpuShares = cpu
	}
	cpus := aws.String(strconv.Itoa(cpuShares))

	// TODO config family
	family := "waypoint-runner"

	s.Update("Registering Task definition: %s", family)
	svc := iam.New(sess)

	roleName := i.config.ExecutionRoleName

	if roleName == "" {
		roleName = "waypoint-server-ecs"
	}

	log.Debug("attempting to retrieve existing role", "role-name", roleName)

	queryInput := &iam.GetRoleInput{
		RoleName: aws.String(roleName),
	}

	var executionRoleArn string

	s.Status("Looking for execution role...")
	getOut, err := svc.GetRole(queryInput)
	if err == nil {
		s.Update("Found existing IAM role to use: %s", roleName)
		executionRoleArn = *getOut.Role.Arn
	}

	if executionRoleArn == "" {
		return fmt.Errorf("unable to find execution role")
	}

	runtime := aws.String("FARGATE")
	containerDefinitions := []*ecs.ContainerDefinition{&def}
	registerTaskDefinitionInput := ecs.RegisterTaskDefinitionInput{
		ContainerDefinitions: containerDefinitions,

		ExecutionRoleArn: aws.String(executionRoleArn),
		Cpu:              cpus,
		Memory:           aws.String(mems),
		Family:           aws.String(family),

		NetworkMode:             aws.String("awsvpc"),
		RequiresCompatibilities: []*string{runtime},
		Tags: []*ecs.Tag{
			{
				Key:   aws.String(runnerName),
				Value: aws.String(runnerName),
			},
		},
	}

	var taskOut *ecs.RegisterTaskDefinitionOutput

	ecsSvc := ecs.New(sess)
	// AWS is eventually consistent so even though we probably created the
	// resources that are referenced by the task definition, it can error out if
	// we try to reference those resources too quickly. So we're forced to guard
	// actions which reference other AWS services with loops like this.
	for i := 0; i < 30; i++ {
		taskOut, err = ecsSvc.RegisterTaskDefinition(&registerTaskDefinitionInput)
		q.Q("-> task registration err:", err)
		if err == nil {
			break
		}

		// if we encounter an unrecoverable error, exit now.
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case "ResourceConflictException":
				return err
			}
		}

		// otherwise sleep and try again
		time.Sleep(2 * time.Second)
	}

	if err != nil {
		return err
	}
	taskDefArn := *taskOut.TaskDefinition.TaskDefinitionArn
	s.Status("Creating Service...")
	log.Debug("creating service", "arn", *taskOut.TaskDefinition.TaskDefinitionArn)

	ec2srv := ec2.New(sess)

	sgName := "waypoint-server-inbound"
	dsg, err := ec2srv.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("group-name"),
				Values: []*string{&sgName},
			},
		},
	})
	if err != nil {
		return err
	}

	var groupId *string

	if len(dsg.SecurityGroups) != 0 {
		groupId = dsg.SecurityGroups[0].GroupId
		s.Update("Using existing security group: %s", sgName)
	} else {
		return fmt.Errorf("could not find waypoint-server-inbound security group")
	}

	// query what subnets and vpc information from the server service
	services, err := ecsSvc.DescribeServices(&ecs.DescribeServicesInput{
		Cluster:  aws.String("waypoint-server"),
		Services: []*string{aws.String(serverName)},
	})
	if err != nil {
		return err
	}

	// should only find one
	service := services.Services[0]
	if service == nil {
		return fmt.Errorf("no waypoint-server service found")
	}

	clusterArn := service.ClusterArn
	subnets := service.NetworkConfiguration.AwsvpcConfiguration.Subnets

	// resume creating the service
	netCfg := &ecs.AwsVpcConfiguration{
		Subnets:        subnets,
		SecurityGroups: []*string{groupId},
	}

	if !i.config.EC2Cluster {
		netCfg.AssignPublicIp = aws.String("ENABLED")
	}

	createServiceInput := &ecs.CreateServiceInput{
		Cluster:        clusterArn,
		DesiredCount:   aws.Int64(1),
		LaunchType:     runtime,
		ServiceName:    aws.String(runnerName),
		TaskDefinition: aws.String(taskDefArn),
		NetworkConfiguration: &ecs.NetworkConfiguration{
			AwsvpcConfiguration: netCfg,
		},
		Tags: []*ecs.Tag{
			{
				Key:   aws.String(runnerName),
				Value: aws.String(runnerName),
			},
		},
	}

	s.Update("Creating ECS Service (%s)", runnerName)

	var servOut *ecs.CreateServiceOutput

	// AWS is eventually consistent so even though we probably created the
	// resources that are referenced by the service, it can error out if we try to
	// reference those resources too quickly. So we're forced to guard actions
	// which reference other AWS services with loops like this.
	for i := 0; i < 30; i++ {
		servOut, err = ecsSvc.CreateService(createServiceInput)
		if err == nil {
			break
		}

		// if we encounter an unrecoverable error, exit now.
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case "AccessDeniedException", "UnsupportedFeatureException",
				"PlatformUnknownException",
				"PlatformTaskDefinitionIncompatibilityException":
				return err
			}
		}

		// otherwise sleep and try again
		time.Sleep(2 * time.Second)
	}

	if err != nil {
		return err
	}

	s.Update("Created ECS Service (%s)", runnerName)
	log.Debug("service started", "arn", servOut.Service.ServiceArn)

	s.Done()

	return nil
}

// UninstallRunner implements Installer.
func (i *ECSInstaller) UninstallRunner(
	ctx context.Context,
	opts *InstallOpts,
) error {
	ui := opts.UI
	// log := opts.Log

	sg := ui.StepGroup()
	defer sg.Wait()

	s := sg.Add("Inspecting ecs cluster...")
	defer func() { s.Abort() }()
	s.Update("No runners installed.")
	s.Done()

	return nil
}

// HasRunner implements Installer.
func (i *ECSInstaller) HasRunner(
	ctx context.Context,
	opts *InstallOpts,
) (bool, error) {

	return false, nil
}

func (i *ECSInstaller) InstallFlags(set *flag.Set) {
	set.StringVar(&flag.StringVar{
		Name:    "ecs-cluster",
		Target:  &i.config.Cluster,
		Usage:   "Configures the Cluster to install into.",
		Default: "waypoint-server",
	})
	set.StringVar(&flag.StringVar{
		Name:    "region",
		Target:  &i.config.Region,
		Usage:   "Configures the region specific things.",
		Default: "us-west-2",
	})
	set.StringSliceVar(&flag.StringSliceVar{
		Name:   "subnets",
		Target: &i.config.Subnets,
		Usage:  "Subnets to install server into.",
	})
	set.StringVar(&flag.StringVar{
		Name:   "ecs-execution-role-name",
		Target: &i.config.ExecutionRoleName,
		Usage:  "Configures the Execution role name to use.",
	})

	set.BoolVar(&flag.BoolVar{
		Name:   "ecs-advertise-internal",
		Target: &i.config.AdvertiseInternal,
		Usage: "Advertise the internal service address rather than the external. " +
			"This is useful if all your deployments will be able to access the private " +
			"service address. This will default to false but will be automatically set to " +
			"true if the external host is detected to be localhost.",
	})

	set.StringVar(&flag.StringVar{
		Name:    "ecs-cpu-request",
		Target:  &i.config.CPU,
		Usage:   "Configures the requested CPU amount for the Waypoint server in ecs.",
		Default: "100m",
	})

	set.StringVar(&flag.StringVar{
		Name:    "ecs-mem-request",
		Target:  &i.config.Memory,
		Usage:   "Configures the requested memory amount for the Waypoint server in ecs.",
		Default: "256Mi",
	})

	set.StringVar(&flag.StringVar{
		Name:    "ecs-server-image",
		Target:  &i.config.ServerImage,
		Usage:   "Docker image for the Waypoint server.",
		Default: defaultServerImage,
	})
}

func (i *ECSInstaller) UpgradeFlags(set *flag.Set) {
	set.BoolVar(&flag.BoolVar{
		Name:   "ecs-advertise-internal",
		Target: &i.config.AdvertiseInternal,
		Usage: "Advertise the internal service address rather than the external. " +
			"This is useful if all your deployments will be able to access the private " +
			"service address. This will default to false but will be automatically set to " +
			"true if the external host is detected to be localhost.",
	})

	set.StringVar(&flag.StringVar{
		Name:    "ecs-cpu-request",
		Target:  &i.config.CPU,
		Usage:   "Configures the requested CPU amount for the Waypoint server in ecs.",
		Default: "100m",
	})

	set.StringVar(&flag.StringVar{
		Name:    "ecs-server-image",
		Target:  &i.config.ServerImage,
		Usage:   "Docker image for the Waypoint server.",
		Default: defaultServerImage,
	})
}

func (i *ECSInstaller) UninstallFlags(set *flag.Set) {
}

type Lifecycle struct {
	Init    func(LifecycleStatus) error
	Run     func(LifecycleStatus) error
	Cleanup func(LifecycleStatus) error
}

type lStatus struct {
	ui   terminal.UI
	sg   terminal.StepGroup
	step terminal.Step
}

func (l *lStatus) Status(str string, args ...interface{}) {
	if l.sg == nil {
		l.sg = l.ui.StepGroup()
	}

	if l.step != nil {
		l.step.Done()
		l.step = nil
	}

	l.step = l.sg.Add(str, args...)
}

func (l *lStatus) Update(str string, args ...interface{}) {
	if l.sg == nil {
		l.sg = l.ui.StepGroup()
	}

	if l.step != nil {
		l.step.Update(str, args...)
	} else {
		l.step = l.sg.Add(str, args)
	}
}

func (l *lStatus) Error(str string, args ...interface{}) {
	if l.sg == nil {
		l.sg = l.ui.StepGroup()
	}

	if l.step != nil {
		l.step.Update(str, args...)
		l.step.Abort()
	} else {
		l.step = l.sg.Add(str, args)
		l.step.Abort()
	}

	l.step = nil
}

func (l *lStatus) Abort() error {
	if l.step != nil {
		l.step.Abort()
		l.step = nil
	}

	if l.sg != nil {
		l.sg.Wait()
		l.sg = nil
	}

	return nil
}

func (l *lStatus) Close() error {
	if l.step != nil {
		l.step.Done()
		l.step = nil
	}

	if l.sg != nil {
		l.sg.Wait()
		l.sg = nil
	}

	return nil
}

func (lf *Lifecycle) Execute(L hclog.Logger, ui terminal.UI) error {
	var l lStatus
	l.ui = ui

	defer l.Close()

	if lf.Init != nil {
		L.Debug("lifecycle init")

		err := lf.Init(&l)
		if err != nil {
			l.Abort()
			return err
		}

	}

	L.Debug("lifecycle run")
	err := lf.Run(&l)
	if err != nil {
		l.Abort()
		return err
	}

	if lf.Cleanup != nil {
		L.Debug("lifecycle cleanup")

		err = lf.Cleanup(&l)
		if err != nil {
			l.Abort()
			return err
		}
	}

	return nil
}

type LifecycleStatus interface {
	Status(str string, args ...interface{})
	Update(str string, args ...interface{})
	Error(str string, args ...interface{})
}

func (i *ECSInstaller) SetupCluster(
	ctx context.Context,
	s LifecycleStatus,
	sess *session.Session,
	ulid string,
) (string, error) {
	ecsSvc := ecs.New(sess)

	cluster := i.config.Cluster
	// shouldn't need this though
	if cluster == "" {
		cluster = "waypoint-server"
	}

	desc, err := ecsSvc.DescribeClusters(&ecs.DescribeClustersInput{
		Clusters: []*string{aws.String(cluster)},
	})
	if err != nil {
		return "", err
	}

	for _, c := range desc.Clusters {
		if *c.ClusterName == cluster && strings.ToLower(*c.Status) == "active" {
			s.Status("Found existing ECS cluster: %s", cluster)
			return cluster, nil
		}
	}

	if i.config.EC2Cluster {
		return "", fmt.Errorf("EC2 clusters can not be automatically created")
	}

	s.Status("Creating new ECS cluster: %s", cluster)

	_, err = ecsSvc.CreateCluster(&ecs.CreateClusterInput{
		ClusterName: aws.String(cluster),
		Tags: []*ecs.Tag{
			{
				Key:   aws.String(serverName),
				Value: aws.String(ulid),
			},
		},
	})

	if err != nil {
		return "", err
	}

	s.Update("Created new ECS cluster: %s", cluster)
	return cluster, nil
}

func (i *ECSInstaller) SetupEFS(
	ctx context.Context,
	s LifecycleStatus,
	sess *session.Session,
	ulid string,
) (string, error) {
	efsSvc := efs.New(sess)
	id, err := component.Id()
	if err != nil {
		return "", err
	}

	s.Status("Creating new EFS file system...")
	fsd, err := efsSvc.CreateFileSystem(&efs.CreateFileSystemInput{
		CreationToken: aws.String(id),
		Encrypted:     aws.Bool(true),
		Tags: []*efs.Tag{
			{
				Key:   aws.String(serverName),
				Value: aws.String(ulid),
			},
		},
	})
	if err != nil {
		return "", err
	}

	s.Update("Created new EFS file system: %s", *fsd.FileSystemId)

	fs, err := efsSvc.DescribeFileSystems(&efs.DescribeFileSystemsInput{
		CreationToken: aws.String(id),
	})
	if err != nil {
		return "", err
	}
	if len(fs.FileSystems) != 1 {
		return "", fmt.Errorf("unexpected count of file systems: %d", len(fs.FileSystems))
	}

	return *fsd.FileSystemId, nil
}

const rolePolicy = `{
  "Version": "2012-10-17",
  "Statement": [
    {
		  "Sid": "",
      "Effect": "Allow",
      "Principal": {
        "Service": "ecs-tasks.amazonaws.com"
      },
      "Action": "sts:AssumeRole"
    }
  ]
}`

var fargateResources = map[int][]int{
	512:  {256},
	1024: {256, 512},
	2048: {256, 512, 1024},
	3072: {512, 1024},
	4096: {512, 1024},
	5120: {1024},
	6144: {1024},
	7168: {1024},
	8192: {1024},
}

func init() {
	for i := 4096; i < 16384; i += 1024 {
		fargateResources[i] = append(fargateResources[i], 2048)
	}

	for i := 8192; i <= 30720; i += 1024 {
		fargateResources[i] = append(fargateResources[i], 4096)
	}
}

type ecsServer struct {
	Url                string
	TaskArn            string
	ServiceArn         string
	HttpTargetGroupArn string
	GRPCTargetGroupArn string
	LoadBalancerArn    string
	Cluster            string
}

type nlb struct {
	lbArn     string
	httpTgArn string
	grpcTgArn string
	publicDNS string
}

func (i *ECSInstaller) Launch(
	ctx context.Context,
	s LifecycleStatus,
	L hclog.Logger,
	ui terminal.UI,
	sess *session.Session,
	efsId string,
	executionRoleArn, taskRoleArn, clusterName, logGroup, ulid string,
) (*ecsServer, error) {
	id, err := component.Id()
	if err != nil {
		return nil, err
	}
	s.Status("Installing Waypoint server into ECS...")

	ecsSvc := ecs.New(sess)

	env := []*ecs.KeyValuePair{
		{
			Name:  aws.String("PORT"),
			Value: aws.String(fmt.Sprint(httpPort)),
		},
	}

	// for k, v := range i.config.Environment {
	// 	env = append(env, &ecs.KeyValuePair{
	// 		Name:  aws.String(k),
	// 		Value: aws.String(v),
	// 	})
	// }

	// var secrets []*ecs.Secret
	// for k, v := range i.config.Secrets {
	// 	secrets = append(secrets, &ecs.Secret{
	// 		Name:      aws.String(k),
	// 		ValueFrom: aws.String(v),
	// 	})
	// }

	// for k, v := range deployConfig.Env() {
	// 	env = append(env, &ecs.KeyValuePair{
	// 		Name:  aws.String(k),
	// 		Value: aws.String(v),
	// 	})
	// }

	defaultStreamPrefix := fmt.Sprintf("waypoint-server-%d", time.Now().Nanosecond())
	logOptions := buildLoggingOptions(
		nil,
		i.config.Region,
		logGroup,
		defaultStreamPrefix,
	)
	var (
		// default resources used for both the Server and its runners. Can be
		// overridden through config flags at install
		defaultResourcesCPU    = 256
		defaultResourcesMemory = 512
	)

	// TODO convert these from config
	cpu := defaultResourcesCPU
	mem := defaultResourcesMemory

	grpcPort, _ := strconv.Atoi(defaultGrpcPort)
	httpPort, _ := strconv.Atoi(defaultHttpPort)

	cmd := []*string{
		aws.String("server"),
		aws.String("run"),
		aws.String("-accept-tos"),
		aws.String("-vvv"),
		aws.String("-db=/waypoint-data/data.db"),
		aws.String(fmt.Sprintf("-listen-grpc=0.0.0.0:%d", grpcPort)),
		aws.String(fmt.Sprintf("-listen-http=0.0.0.0:%d", httpPort)),
	}

	def := ecs.ContainerDefinition{
		Essential: aws.Bool(true),
		Command:   cmd,
		Name:      aws.String("waypoint-server"),
		Image:     aws.String(i.config.ServerImage),
		PortMappings: []*ecs.PortMapping{
			{
				// TODO: configurable port
				ContainerPort: aws.Int64(int64(httpPort)),
			},
			{
				// TODO: configurable port
				ContainerPort: aws.Int64(int64(grpcPort)),
			},
		},
		Environment: env,
		// TODO: These are optional for Fargate
		Memory: utils.OptionalInt64(int64(mem)),
		LogConfiguration: &ecs.LogConfiguration{
			LogDriver: aws.String("awslogs"),
			Options:   logOptions,
		},
		// TODO: configure this
		MountPoints: []*ecs.MountPoint{
			{
				SourceVolume:  aws.String("myefs"),
				ContainerPath: aws.String("/waypoint-data"),
				ReadOnly:      aws.Bool(false),
			},
		},
	}

	s.Status("Setting up networking resources...")
	var subnets []*string
	if len(i.config.Subnets) == 0 {
		s.Update("Using default subnets for Service networking")
		subnets, err = defaultSubnets(ctx, sess)
		if err != nil {
			return nil, err
		}
	} else {
		subnets = make([]*string, len(i.config.Subnets))
		for j := range i.config.Subnets {
			subnets[j] = &i.config.Subnets[j]
		}
		s.Update("Using provided subnets for Service networking")
	}

	ec2srv := ec2.New(sess)

	subnetInfo, err := ec2srv.DescribeSubnets(&ec2.DescribeSubnetsInput{
		SubnetIds: subnets,
	})
	if err != nil {
		return nil, err
	}

	vpcId := subnetInfo.Subnets[0].VpcId

	sgName := "waypoint-server-inbound"
	s.Status("Setting up security group...")
	sgecsport, err := createSG(ctx, s, sess, sgName, vpcId, ulid, httpPort, grpcPort)
	if err != nil {
		return nil, err
	}

	s.Status("Creating Network resources...")
	nlb, err := createNLB(
		ctx, s, L, sess,
		vpcId,
		aws.Int64(int64(grpcPort)),
		subnets,
		ulid,
	)
	if err != nil {
		return nil, err
	}
	s.Update("Created Network resources")

	// Create mount points for the EFS file system. The EFS mount targets need to
	// existin in a 1:1 pair with the subnets in use.
	s.Status("Creating EFS Mount targets...")
	efsSvc := efs.New(sess)

	L.Debug("polling for EFS", "FileSystemID", efsId)

EFSLOOP:
	for i := 0; i < 10; i++ {
		fsList, err := efsSvc.DescribeFileSystems(&efs.DescribeFileSystemsInput{
			FileSystemId: aws.String(efsId),
		})
		if err != nil {
			return nil, err
		}
		if len(fsList.FileSystems) == 0 {
			return nil, fmt.Errorf("file system (%s) not found", efsId)
		}
		// check the status of the first one
		fs := fsList.FileSystems[0]
		switch *fs.LifeCycleState {
		case efs.LifeCycleStateDeleted, efs.LifeCycleStateDeleting:
			return nil, fmt.Errorf("files system is deleting/deleted")
		case efs.LifeCycleStateAvailable:
			break EFSLOOP
		}
		time.Sleep(1 * time.Second)
	}

	// poll for available
	for _, sub := range subnets {
		_, err := efsSvc.CreateMountTarget(&efs.CreateMountTargetInput{
			FileSystemId:   aws.String(efsId),
			SecurityGroups: []*string{sgecsport},
			SubnetId:       sub,
			// Mount Targets do not support tags directly
		})
		if err != nil {
			return nil, fmt.Errorf("error creating mount target: %w", err)
		}
	}

	// create EFS access points
	s.Update("Creating EFS Access Point...")
	// TODO: move this to SetupEFS and share the access point returned
	// these are the User and Group ids created in the Waypoint Server Dockerfile
	uid := aws.Int64(int64(100))
	gid := aws.Int64(int64(1000))
	accessPoint, err := efsSvc.CreateAccessPoint(&efs.CreateAccessPointInput{
		FileSystemId: aws.String(efsId),
		PosixUser: &efs.PosixUser{
			Uid: uid,
			Gid: gid,
		},
		RootDirectory: &efs.RootDirectory{
			CreationInfo: &efs.CreationInfo{
				OwnerUid:    uid,
				OwnerGid:    gid,
				Permissions: aws.String("755"),
			},
			Path: aws.String("/waypointserverdata"),
		},
		Tags: []*efs.Tag{
			{
				Key:   aws.String(serverName),
				Value: aws.String(ulid),
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("error creating access point: %w", err)
	}
	// loop until all mount targets are ready, or the first container can have
	// issues starting

	// create EFS access points
	s.Update("Waiting for EFS mount targets to become available...")
	var available int
	for i := 0; 1 < 30; i++ {
		mtgs, err := efsSvc.DescribeMountTargets(&efs.DescribeMountTargetsInput{
			AccessPointId: accessPoint.AccessPointId,
		})
		if err != nil {
			return nil, err
		}

		for _, m := range mtgs.MountTargets {
			if *m.LifeCycleState == efs.LifeCycleStateAvailable {
				available++
			}
		}
		if available == len(subnets) {
			break
		}

		available = 0
		time.Sleep(5 * time.Second)
		continue
	}

	if available != len(subnets) {
		return nil, fmt.Errorf("not enough mount targets found")
	}

	time.Sleep(3 * time.Second)

	s.Status("Creating ECS Service and Tasks...")
	L.Debug("registering task definition", "id", id)

	var cpuShares int

	runtime := aws.String("FARGATE")
	if i.config.EC2Cluster {
		runtime = aws.String("EC2")
		cpuShares = cpu
	} else {
		if mem == 0 {
			return nil, fmt.Errorf("Memory value required for fargate")
		}
		cpuValues, ok := fargateResources[mem]
		if !ok {
			var (
				allValues  []int
				goodValues []string
			)

			for k := range fargateResources {
				allValues = append(allValues, k)
			}

			sort.Ints(allValues)

			for _, k := range allValues {
				goodValues = append(goodValues, strconv.Itoa(k))
			}

			return nil, fmt.Errorf("Invalid memory value: %d (valid values: %s)",
				mem, strings.Join(goodValues, ", "))
		}

		if cpu == 0 {
			cpuShares = cpuValues[0]
		} else {
			var (
				valid      bool
				goodValues []string
			)

			for _, c := range cpuValues {
				goodValues = append(goodValues, strconv.Itoa(c))
				if c == cpu {
					valid = true
					break
				}
			}

			if !valid {
				return nil, fmt.Errorf("Invalid cpu value: %d (valid values: %s)",
					mem, strings.Join(goodValues, ", "))
			}

			cpuShares = cpu
		}
	}

	cpus := aws.String(strconv.Itoa(cpuShares))
	// on EC2 launch type, `Cpu` is an optional field, so we leave it nil if it is 0
	if i.config.EC2Cluster && cpuShares == 0 {
		cpus = nil
	}

	mems := strconv.Itoa(mem)

	// TODO config family
	family := "waypoint-server"

	s.Update("Registering Task definition: %s", family)

	containerDefinitions := []*ecs.ContainerDefinition{&def}

	registerTaskDefinitionInput := ecs.RegisterTaskDefinitionInput{
		ContainerDefinitions: containerDefinitions,

		ExecutionRoleArn: aws.String(executionRoleArn),
		Cpu:              cpus,
		Memory:           aws.String(mems),
		Family:           aws.String(family),

		NetworkMode:             aws.String("awsvpc"),
		RequiresCompatibilities: []*string{runtime},
		Tags: []*ecs.Tag{
			{
				Key:   aws.String(serverName),
				Value: aws.String(ulid),
			},
		},
		// TODO: configurable / create EFS
		Volumes: []*ecs.Volume{
			{
				Name: aws.String("myefs"),
				EfsVolumeConfiguration: &ecs.EFSVolumeConfiguration{
					TransitEncryption: aws.String(ecs.EFSTransitEncryptionEnabled),
					FileSystemId:      accessPoint.FileSystemId,
					AuthorizationConfig: &ecs.EFSAuthorizationConfig{
						AccessPointId: accessPoint.AccessPointId,
					},
				},
			},
		},
	}

	var taskOut *ecs.RegisterTaskDefinitionOutput

	// AWS is eventually consistent so even though we probably created the
	// resources that are referenced by the task definition, it can error out if
	// we try to reference those resources too quickly. So we're forced to guard
	// actions which reference other AWS services with loops like this.
	for i := 0; i < 30; i++ {
		taskOut, err = ecsSvc.RegisterTaskDefinition(&registerTaskDefinitionInput)
		if err == nil {
			break
		}

		// if we encounter an unrecoverable error, exit now.
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case "ResourceConflictException":
				return nil, err
			}
		}

		// otherwise sleep and try again
		time.Sleep(2 * time.Second)
	}

	if err != nil {
		return nil, err
	}

	// TODO: check service name
	// serviceName := fmt.Sprintf("%s-%s", "waypoint-server", id)
	serviceName := "waypoint-server"

	// We have to clamp at a length of 32 because the Name field to CreateTargetGroup
	// requires that the name is 32 characters or less.
	if len(serviceName) > 27 {
		serviceName = serviceName[:27]
		L.Debug("using a shortened value for service name due to AWS's length limits", "serviceName", serviceName)
	}

	taskDefArn := *taskOut.TaskDefinition.TaskDefinitionArn

	// Create the service
	s.Update("Creating Service...")
	L.Debug("creating service", "arn", *taskOut.TaskDefinition.TaskDefinitionArn)

	// resume creating the service
	netCfg := &ecs.AwsVpcConfiguration{
		Subnets:        subnets,
		SecurityGroups: []*string{sgecsport},
	}

	if !i.config.EC2Cluster {
		netCfg.AssignPublicIp = aws.String("ENABLED")
	}

	createServiceInput := &ecs.CreateServiceInput{
		Cluster:                       &clusterName,
		DesiredCount:                  aws.Int64(1),
		LaunchType:                    runtime,
		ServiceName:                   aws.String(serviceName),
		TaskDefinition:                aws.String(taskDefArn),
		HealthCheckGracePeriodSeconds: aws.Int64(int64(600)),
		NetworkConfiguration: &ecs.NetworkConfiguration{
			AwsvpcConfiguration: netCfg,
		},
		Tags: []*ecs.Tag{
			{
				Key:   aws.String(serverName),
				Value: aws.String(ulid),
			},
		},
		LoadBalancers: []*ecs.LoadBalancer{
			{
				ContainerName:  aws.String("waypoint-server"),
				ContainerPort:  aws.Int64(int64(httpPort)),
				TargetGroupArn: aws.String(nlb.httpTgArn),
			},
			{
				ContainerName:  aws.String("waypoint-server"),
				ContainerPort:  aws.Int64(int64(grpcPort)),
				TargetGroupArn: aws.String(nlb.grpcTgArn),
			},
		},
	}

	// createServiceInput.SetLoadBalancers([]*ecs.LoadBalancer{
	// 	{
	// 		ContainerName:  aws.String("waypoint-server"),
	// 		ContainerPort:  aws.Int64(int64(httpPort)),
	// 		TargetGroupArn: aws.String(nlb.httpTgArn),
	// 	},
	// 	{
	// 		ContainerName:  aws.String("waypoint-server"),
	// 		ContainerPort:  aws.Int64(int64(grpcPort)),
	// 		TargetGroupArn: aws.String(nlb.grpcTgArn),
	// 	},
	// })

	s.Status("Creating ECS Service (%s, cluster-name: %s)", serviceName, clusterName)

	var servOut *ecs.CreateServiceOutput

	// AWS is eventually consistent so even though we probably created the
	// resources that are referenced by the service, it can error out if we try to
	// reference those resources too quickly. So we're forced to guard actions
	// which reference other AWS services with loops like this.
	for i := 0; i < 30; i++ {
		servOut, err = ecsSvc.CreateService(createServiceInput)
		if err == nil {
			break
		}

		// if we encounter an unrecoverable error, exit now.
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case "AccessDeniedException", "UnsupportedFeatureException",
				"PlatformUnknownException",
				"PlatformTaskDefinitionIncompatibilityException":
				return nil, err
			}
		}

		// otherwise sleep and try again
		time.Sleep(2 * time.Second)
	}

	if err != nil {
		return nil, err
	}

	s.Update("Created ECS Service (%s, cluster-name: %s)", serviceName, clusterName)
	L.Debug("service started", "arn", servOut.Service.ServiceArn)

	dep := &ecsServer{
		Url:                nlb.publicDNS,
		Cluster:            clusterName,
		TaskArn:            taskDefArn,
		HttpTargetGroupArn: nlb.httpTgArn,
		ServiceArn:         *servOut.Service.ServiceArn,
	}

	s.Update("Waiting for target group to be healthy...")
	elbsrv := elbv2.New(sess)
	var healthy bool
	for i := 0; i < 80; i++ {
		health, err := elbsrv.DescribeTargetHealth(&elbv2.DescribeTargetHealthInput{
			TargetGroupArn: &nlb.httpTgArn,
		})
		if err != nil {
			return nil, err
		}
		if len(health.TargetHealthDescriptions) == 0 {
			// return nil, fmt.Errorf("too many target healths: %d", len(health.TargetHealthDescriptions))
		} else {

			// grab the first, most recent
			hd := health.TargetHealthDescriptions[0]

			if hd.TargetHealth.State != nil && *hd.TargetHealth.State == elbv2.TargetHealthStateEnumHealthy {
				healthy = true
				break
			}
		}
		time.Sleep(5 * time.Second)
	}

	if !healthy {
		return nil, fmt.Errorf("no healthy target group")
	}
	s.Status("Service launched!")
	return dep, nil
}

func createSG(
	ctx context.Context,
	s LifecycleStatus,
	sess *session.Session,
	name string,
	vpcId *string,
	ulid string,
	ports ...int,
) (*string, error) {
	ec2srv := ec2.New(sess)

	dsg, err := ec2srv.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("group-name"),
				Values: []*string{aws.String(name)},
			},
			{
				Name:   aws.String("vpc-id"),
				Values: []*string{vpcId},
			},
		},
	})
	if err != nil {
		return nil, err
	}

	var groupId *string

	if len(dsg.SecurityGroups) != 0 {
		groupId = dsg.SecurityGroups[0].GroupId
		s.Update("Using existing security group: %s", name)
	} else {
		s.Update("Creating security group: %s", name)
		out, err := ec2srv.CreateSecurityGroup(&ec2.CreateSecurityGroupInput{
			Description: aws.String("created by waypoint"),
			GroupName:   aws.String(name),
			VpcId:       vpcId,
			TagSpecifications: []*ec2.TagSpecification{
				{
					ResourceType: aws.String(ec2.ResourceTypeSecurityGroup),
					Tags: []*ec2.Tag{
						{
							Key:   aws.String(serverName),
							Value: aws.String(ulid),
						},
					},
				},
			},
		})
		if err != nil {
			return nil, err
		}

		groupId = out.GroupId
		s.Update("Created security group: %s", name)
	}

	s.Update("Authorizing ports to security group")
	// Port 2049 is the port for accessing EFS file systems over NFS
	ports = append(ports, 2049, 80)
	for _, port := range ports {
		_, err = ec2srv.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
			CidrIp:     aws.String("0.0.0.0/0"),
			FromPort:   aws.Int64(int64(port)),
			ToPort:     aws.Int64(int64(port)),
			GroupId:    groupId,
			IpProtocol: aws.String("tcp"),
		})
	}

	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case "InvalidPermission.Duplicate":
				// fine, means we already added it.
			default:
				return nil, err
			}
		} else {
			return nil, err
		}
	}

	return groupId, nil
}

func (i *ECSInstaller) SetupLogs(
	ctx context.Context,
	s LifecycleStatus,
	L hclog.Logger,
	sess *session.Session,
	ulid string,
	logGroup string,
) (string, error) {
	cwl := cloudwatchlogs.New(sess)

	s.Status("Creating CloudWatchLogs groups...")

	groups, err := cwl.DescribeLogGroups(&cloudwatchlogs.DescribeLogGroupsInput{
		Limit:              aws.Int64(1),
		LogGroupNamePrefix: aws.String(logGroup),
	})
	if err != nil {
		return "", err
	}

	if len(groups.LogGroups) == 0 {
		s.Status("Creating CloudWatchLogs group to store logs in: %s", logGroup)

		L.Debug("creating log group", "group", logGroup)
		_, err = cwl.CreateLogGroup(&cloudwatchlogs.CreateLogGroupInput{
			LogGroupName: aws.String(logGroup),
		})
		if err != nil {
			return "", err
		}

		s.Update("Created CloudWatchLogs group to store logs in: %s", logGroup)
	}

	return logGroup, nil
}

func buildLoggingOptions(
	lo *Logging,
	region string,
	logGroup string,
	defaultStreamPrefix string,
) map[string]*string {
	result := map[string]*string{
		"awslogs-region":        aws.String(region),
		"awslogs-group":         aws.String(logGroup),
		"awslogs-stream-prefix": aws.String(defaultStreamPrefix),
	}

	if lo != nil {
		// We receive the error `Log driver awslogs disallows options:
		// awslogs-endpoint` when setting `awslogs-endpoint`, so that is not
		// included here of the available options
		// https://docs.aws.amazon.com/AmazonECS/latest/developerguide/using_awslogs.html
		result["awslogs-datetime-format"] = aws.String(lo.DateTimeFormat)
		result["awslogs-multiline-pattern"] = aws.String(lo.MultilinePattern)
		result["mode"] = aws.String(lo.Mode)
		result["max-buffer-size"] = aws.String(lo.MaxBufferSize)

		if lo.CreateGroup {
			result["awslogs-create-group"] = aws.String("true")
		}
		if lo.StreamPrefix != "" {
			result["awslogs-stream-prefix"] = aws.String(lo.StreamPrefix)
		}
	}

	for k, v := range result {
		if *v == "" {
			delete(result, k)
		}
	}

	return result
}

type Logging struct {
	CreateGroup bool `hcl:"create_group,optional"`

	StreamPrefix string `hcl:"stream_prefix,optional"`

	DateTimeFormat string `hcl:"datetime_format,optional"`

	MultilinePattern string `hcl:"multiline_pattern,optional"`

	Mode string `hcl:"mode,optional"`

	MaxBufferSize string `hcl:"max_buffer_size,optional"`
}

func (i *ECSInstaller) SetupExecutionRole(
	ctx context.Context,
	s LifecycleStatus,
	L hclog.Logger,
	sess *session.Session,
	ulid string,
) (string, error) {
	svc := iam.New(sess)

	roleName := i.config.ExecutionRoleName

	if roleName == "" {
		roleName = "waypoint-server-ecs"
	}

	// role names have to be 64 characters or less, and the client side doesn't
	// validate this.
	if len(roleName) > 64 {
		roleName = roleName[:64]
		L.Debug("using a shortened value for role name due to AWS's length limits", "roleName", roleName)
	}

	L.Debug("attempting to retrieve existing role", "role-name", roleName)

	queryInput := &iam.GetRoleInput{
		RoleName: aws.String(roleName),
	}

	getOut, err := svc.GetRole(queryInput)
	if err == nil {
		s.Status("Found existing IAM role to use: %s", roleName)
		return *getOut.Role.Arn, nil
	}

	L.Debug("creating new role")
	s.Status("Creating IAM role: %s", roleName)

	input := &iam.CreateRoleInput{
		AssumeRolePolicyDocument: aws.String(rolePolicy),
		Path:                     aws.String("/"),
		RoleName:                 aws.String(roleName),
		Tags: []*iam.Tag{
			{
				Key:   aws.String(serverName),
				Value: aws.String(ulid),
			},
		},
	}

	result, err := svc.CreateRole(input)
	if err != nil {
		return "", err
	}

	roleArn := *result.Role.Arn

	L.Debug("created new role", "arn", roleArn)

	aInput := &iam.AttachRolePolicyInput{
		RoleName:  aws.String(roleName),
		PolicyArn: aws.String("arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"),
	}

	_, err = svc.AttachRolePolicy(aInput)
	if err != nil {
		return "", err
	}

	L.Debug("attached execution role policy")

	s.Update("Created IAM role: %s", roleName)
	return roleArn, nil
}

// creates a network load balancer for grpc and http
func createNLB(
	ctx context.Context,
	s LifecycleStatus,
	L hclog.Logger,
	sess *session.Session,
	vpcId *string,
	grpcPort *int64,
	subnets []*string,
	ulid string,
) (serverNLB *nlb, err error) {

	s.Update("Creating NLB target groups")
	elbsrv := elbv2.New(sess)

	ctgGPRC, err := elbsrv.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name:       aws.String("waypoint-server-grpc"),
		Port:       grpcPort,
		Protocol:   aws.String("TCP"),
		TargetType: aws.String("ip"),
		// HealthCheckIntervalSeconds: aws.Int64(int64(30)),
		// HealthCheckTimeoutSeconds:  aws.Int64(int64(10)),
		// HealthyThresholdCount:      aws.Int64(int64(3)),
		// UnhealthyThresholdCount:    aws.Int64(int64(3)),
		VpcId: vpcId,
		Tags: []*elbv2.Tag{
			{
				Key:   aws.String(serverName),
				Value: aws.String(ulid),
			},
		},
	})
	if err != nil {
		return nil, err
	}

	htgGPRC, err := elbsrv.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name:                aws.String("waypoint-server-http"),
		HealthCheckProtocol: aws.String(elbv2.ProtocolEnumHttps),
		HealthCheckPath:     aws.String("/auth"),
		// HealthCheckPort:            aws.String("9702"),
		HealthCheckIntervalSeconds: aws.Int64(int64(30)),
		HealthCheckTimeoutSeconds:  aws.Int64(int64(10)),
		Port:                       aws.Int64(int64(9702)),
		Protocol:                   aws.String("TCP"),
		TargetType:                 aws.String("ip"),
		VpcId:                      vpcId,
		Tags: []*elbv2.Tag{
			{
				Key:   aws.String(serverName),
				Value: aws.String(ulid),
			},
		},
	})
	if err != nil {
		return nil, err
	}

	httpTgArn := htgGPRC.TargetGroups[0].TargetGroupArn
	grpcTgArn := ctgGPRC.TargetGroups[0].TargetGroupArn

	// Create the load balancer OR modify the existing one to have this new target
	// group but with a weight of 0

	htgs := []*elbv2.TargetGroupTuple{
		{
			TargetGroupArn: httpTgArn,
		},
	}
	gtgs := []*elbv2.TargetGroupTuple{
		{
			TargetGroupArn: grpcTgArn,
		},
	}

	var certs []*elbv2.Certificate

	var lb *elbv2.LoadBalancer

	lbName := "waypoint-server-nlb"
	dlb, err := elbsrv.DescribeLoadBalancers(&elbv2.DescribeLoadBalancersInput{
		Names: []*string{&lbName},
	})
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case elbv2.ErrCodeLoadBalancerNotFoundException:
				// fine, means we'll create it.
			default:
				return nil, err
			}
		} else {
			return nil, err
		}
	}

	if dlb != nil && len(dlb.LoadBalancers) > 0 {
		lb = dlb.LoadBalancers[0]
		s.Update("Using existing NLB %s (%s, dns-name: %s)",
			lbName, *lb.LoadBalancerArn, *lb.DNSName)
	} else {
		s.Update("Creating new NLB: %s", lbName)

		scheme := elbv2.LoadBalancerSchemeEnumInternetFacing

		clb, err := elbsrv.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
			Name:    aws.String(lbName),
			Subnets: subnets,
			// SecurityGroups: []*string{sgWebId},
			Scheme: &scheme,
			Type:   aws.String(elbv2.LoadBalancerTypeEnumNetwork),
			Tags: []*elbv2.Tag{
				{
					Key:   aws.String(serverName),
					Value: aws.String(ulid),
				},
			},
		})
		if err != nil {
			return nil, err
		}

		s.Update("Waiting on NLB to be active...")
		lb = clb.LoadBalancers[0]
		for i := 0; 1 < 70; i++ {
			clbd, err := elbsrv.DescribeLoadBalancers(&elbv2.DescribeLoadBalancersInput{
				LoadBalancerArns: []*string{lb.LoadBalancerArn},
			})
			if err != nil {
				return nil, err
			}
			lb = clbd.LoadBalancers[0]
			if lb.State != nil && *lb.State.Code == elbv2.LoadBalancerStateEnumActive {
				break
			}
			if lb.State != nil && *lb.State.Code == elbv2.LoadBalancerStateEnumFailed {
				return nil, fmt.Errorf("failed to create NLB")
			}

			time.Sleep(5 * time.Second)
		}

		if *lb.State.Code != elbv2.LoadBalancerStateEnumActive {
			return nil, fmt.Errorf("failed to create NLB in time, last state: (%s)", *lb.State.Code)
		}

		s.Update("Created new NLB: %s (dns-name: %s)", lbName, *lb.DNSName)
	}

	s.Update("Creating new NLB Listener")

	L.Info("load-balancer defined", "dns-name", *lb.DNSName)

	_, err = elbsrv.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lb.LoadBalancerArn,
		Port:            grpcPort,
		Protocol:        aws.String("TCP"),
		Certificates:    certs,
		DefaultActions: []*elbv2.Action{
			{
				ForwardConfig: &elbv2.ForwardActionConfig{
					TargetGroups: gtgs,
				},
				Type: aws.String("forward"),
			},
		},
		Tags: []*elbv2.Tag{
			{
				Key:   aws.String(serverName),
				Value: aws.String(ulid),
			},
		},
	})
	if err != nil {
		return nil, err
	}

	_, err = elbsrv.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lb.LoadBalancerArn,
		Port:            aws.Int64(int64(9702)),
		Protocol:        aws.String("TCP"),
		Certificates:    certs,
		DefaultActions: []*elbv2.Action{
			{
				ForwardConfig: &elbv2.ForwardActionConfig{
					TargetGroups: htgs,
				},
				Type: aws.String("forward"),
			},
		},
		Tags: []*elbv2.Tag{
			{
				Key:   aws.String(serverName),
				Value: aws.String(ulid),
			},
		},
	})
	if err != nil {
		return nil, err
	}

	return &nlb{
		lbArn:     *lb.LoadBalancerArn,
		httpTgArn: *httpTgArn,
		grpcTgArn: *grpcTgArn,
		publicDNS: *lb.DNSName,
	}, nil
}

func defaultSubnets(ctx context.Context, sess *session.Session) ([]*string, error) {
	svc := ec2.New(sess)

	desc, err := svc.DescribeSubnets(&ec2.DescribeSubnetsInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("default-for-az"),
				Values: []*string{aws.String("true")},
			},
		},
	})
	if err != nil {
		return nil, err
	}

	var subnets []*string

	for _, subnet := range desc.Subnets {
		subnets = append(subnets, subnet.SubnetId)
	}

	return subnets, nil
}
