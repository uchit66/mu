package workflows

import (
	"fmt"
	"github.com/stelligent/mu/common"
	"github.com/stelligent/mu/templates"
	"strconv"
	"strings"
)

// NewServiceDeployer create a new workflow for deploying a service in an environment
func NewServiceDeployer(ctx *common.Context, environmentName string, tag string) Executor {

	workflow := new(serviceWorkflow)
	workflow.codeRevision = ctx.Config.Repo.Revision
	workflow.repoName = ctx.Config.Repo.Slug

	ecsImportParams := make(map[string]string)

	return newWorkflow(
		workflow.serviceLoader(ctx, tag),
		workflow.serviceEnvironmentLoader(environmentName, ctx.StackManager, ecsImportParams, ctx.ElbManager, ctx.ParamManager),
		workflow.serviceRepoUpserter(&ctx.Config.Service, ctx.StackManager, ctx.StackManager),
		workflow.serviceDeployer(&ctx.Config.Service, ecsImportParams, environmentName, ctx.StackManager, ctx.StackManager),
	)
}

func getMaxPriority(elbRuleLister common.ElbRuleLister, listenerArn string) int {
	rules, err := elbRuleLister.ListRules(listenerArn)
	if err != nil {
		log.Debugf("Error finding max priority for listener '%s': %v", listenerArn, err)
		return 0
	}
	maxPriority := 0
	for _, rule := range rules {
		priority, _ := strconv.Atoi(common.StringValue(rule.Priority))
		if priority > maxPriority {
			maxPriority = priority
		}
	}
	return maxPriority
}

func (workflow *serviceWorkflow) serviceEnvironmentLoader(environmentName string, stackWaiter common.StackWaiter, ecsImportParams map[string]string, elbRuleLister common.ElbRuleLister, paramGetter common.ParamGetter) Executor {
	return func() error {
		lbStackName := common.CreateStackName(common.StackTypeLoadBalancer, environmentName)
		lbStack := stackWaiter.AwaitFinalStatus(lbStackName)

		ecsStackName := common.CreateStackName(common.StackTypeCluster, environmentName)
		ecsStack := stackWaiter.AwaitFinalStatus(ecsStackName)

		if lbStack != nil {
			workflow.envProvider = common.EnvProvider(lbStack.Tags["provider"])
		} else if ecsStack != nil {
			workflow.envProvider = common.EnvProvider(ecsStack.Tags["provider"])
		} else {
			return fmt.Errorf("Unable to find stack '%s' for environment '%s'", ecsStackName, environmentName)
		}

		ecsImportParams["VpcId"] = fmt.Sprintf("%s-VpcId", ecsStackName)
		ecsImportParams["EcsCluster"] = fmt.Sprintf("%s-EcsCluster", ecsStackName)

		nextAvailablePriority := 0
		if lbStack.Outputs["ElbHttpListenerArn"] != "" {
			ecsImportParams["ElbHttpListenerArn"] = fmt.Sprintf("%s-ElbHttpListenerArn", ecsStackName)
			nextAvailablePriority = 1 + getMaxPriority(elbRuleLister, lbStack.Outputs["ElbHttpListenerArn"])
		}
		if lbStack.Outputs["ElbHttpsListenerArn"] != "" {
			ecsImportParams["ElbHttpsListenerArn"] = fmt.Sprintf("%s-ElbHttpsListenerArn", ecsStackName)
			if nextAvailablePriority == 0 {
				nextAvailablePriority = 1 + getMaxPriority(elbRuleLister, lbStack.Outputs["ElbHttpsListenerArn"])
			}
		}

		dbStackName := common.CreateStackName(common.StackTypeDatabase, workflow.serviceName, environmentName)
		dbStack := stackWaiter.AwaitFinalStatus(dbStackName)
		if dbStack != nil {
			ecsImportParams["DatabaseName"] = dbStack.Outputs["DatabaseName"]
			ecsImportParams["DatabaseEndpointAddress"] = dbStack.Outputs["DatabaseEndpointAddress"]
			ecsImportParams["DatabaseEndpointPort"] = dbStack.Outputs["DatabaseEndpointPort"]
			ecsImportParams["DatabaseMasterUsername"] = dbStack.Outputs["DatabaseMasterUsername"]

			dbPass, _ := paramGetter.GetParam(fmt.Sprintf("%s-%s", dbStackName, "DatabaseMasterPassword"))
			ecsImportParams["DatabaseMasterPassword"] = dbPass
		}

		svcStackName := common.CreateStackName(common.StackTypeService, workflow.serviceName, environmentName)
		svcStack := stackWaiter.AwaitFinalStatus(svcStackName)
		if workflow.priority > 0 {
			ecsImportParams["ListenerRulePriority"] = strconv.Itoa(workflow.priority)
		} else if svcStack != nil {
			// no value in config, and this is an update...use prior value
			ecsImportParams["ListenerRulePriority"] = ""
		} else {
			// no value in config, and this is a create...use next available
			ecsImportParams["ListenerRulePriority"] = strconv.Itoa(nextAvailablePriority)
		}

		return nil
	}
}

func (workflow *serviceWorkflow) serviceDeployer(service *common.Service, stackParams map[string]string, environmentName string, stackUpserter common.StackUpserter, stackWaiter common.StackWaiter) Executor {
	return func() error {
		log.Debugf("Deploying service with provider '%s'", workflow.envProvider)
		if workflow.envProvider == "" || strings.EqualFold(string(workflow.envProvider), string(common.EnvProviderEcs)) {
			return workflow.serviceEcsDeployer(service, stackParams, environmentName, stackUpserter, stackWaiter)
		} else if strings.EqualFold(string(workflow.envProvider), string(common.EnvProviderEc2)) {
			return workflow.serviceEc2Deployer(service, stackParams, environmentName, stackUpserter, stackWaiter)
		} else {
			return fmt.Errorf("Unknown provider '%s'", workflow.envProvider)
		}
	}
}

func (workflow *serviceWorkflow) serviceEc2Deployer(service *common.Service, stackParams map[string]string, environmentName string, stackUpserter common.StackUpserter, stackWaiter common.StackWaiter) error {
	log.Noticef("Deploying service '%s' to '%s'", workflow.serviceName, environmentName)

	return nil
}

func (workflow *serviceWorkflow) serviceEcsDeployer(service *common.Service, stackParams map[string]string, environmentName string, stackUpserter common.StackUpserter, stackWaiter common.StackWaiter) error {
	log.Noticef("Deploying service '%s' to '%s' from '%s'", workflow.serviceName, environmentName, workflow.serviceImage)

	stackParams["ServiceName"] = workflow.serviceName
	stackParams["ImageUrl"] = workflow.serviceImage
	if service.Port != 0 {
		stackParams["ServicePort"] = strconv.Itoa(service.Port)
	}
	if service.HealthEndpoint != "" {
		stackParams["ServiceHealthEndpoint"] = service.HealthEndpoint
	}
	if service.CPU != 0 {
		stackParams["ServiceCpu"] = strconv.Itoa(service.CPU)
	}
	if service.Memory != 0 {
		stackParams["ServiceMemory"] = strconv.Itoa(service.Memory)
	}
	if service.DesiredCount != 0 {
		stackParams["ServiceDesiredCount"] = strconv.Itoa(service.DesiredCount)
	}
	if len(service.PathPatterns) > 0 {
		stackParams["PathPattern"] = strings.Join(service.PathPatterns, ",")
	}

	svcStackName := common.CreateStackName(common.StackTypeService, workflow.serviceName, environmentName)

	resolveServiceEnvironment(service, environmentName)
	overrides := common.GetStackOverrides(svcStackName)
	template, err := templates.NewTemplate("service.yml", service, overrides)
	if err != nil {
		return err
	}

	err = stackUpserter.UpsertStack(svcStackName, template, stackParams, buildServiceTags(workflow.serviceName, environmentName, workflow.envProvider, common.StackTypeService, workflow.codeRevision, workflow.repoName))
	if err != nil {
		return err
	}
	log.Debugf("Waiting for stack '%s' to complete", svcStackName)
	stack := stackWaiter.AwaitFinalStatus(svcStackName)
	if stack == nil {
		return fmt.Errorf("Unable to create stack %s", svcStackName)
	}
	if strings.HasSuffix(stack.Status, "ROLLBACK_COMPLETE") || !strings.HasSuffix(stack.Status, "_COMPLETE") {
		return fmt.Errorf("Ended in failed status %s %s", stack.Status, stack.StatusReason)
	}

	return nil
}

func resolveServiceEnvironment(service *common.Service, environment string) {
	for key, value := range service.Environment {
		switch value.(type) {
		case map[interface{}]interface{}:
			found := false
			for env, v := range value.(map[interface{}]interface{}) {
				if env.(string) == environment {
					service.Environment[key] = v.(string)
					found = true
				}
			}
			if found != true {
				service.Environment[key] = ""
			}
		case string:
			// do nothing
		default:
			log.Warningf("Unable to resolve environment '%s': %v", key, value)
		}

	}
}

func buildServiceTags(serviceName string, environmentName string, envProvider common.EnvProvider, stackType common.StackType, codeRevision string, repoName string) map[string]string {
	return map[string]string{
		"type":        string(stackType),
		"environment": environmentName,
		"provider":    string(envProvider),
		"service":     serviceName,
		"revision":    codeRevision,
		"repo":        repoName,
	}
}
