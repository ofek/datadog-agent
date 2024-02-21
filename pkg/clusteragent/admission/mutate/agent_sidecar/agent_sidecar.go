// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build kubeapiserver

// Package agentsidecar defines the mutation logic for the agentsidecar webhook
package agentsidecar

import (
	"errors"
	"fmt"
	dca_ac "github.com/DataDog/datadog-agent/pkg/clusteragent/admission/mutate"
	"github.com/DataDog/datadog-agent/pkg/config"
	apiCommon "github.com/DataDog/datadog-agent/pkg/util/kubernetes/apiserver/common"
	"github.com/DataDog/datadog-agent/pkg/util/log"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/dynamic"
	k8s "k8s.io/client-go/kubernetes"
	"os"
)

// InjectAgentSidecar handles mutating pod requests for the agentsidecat webhook
func InjectAgentSidecar(rawPod []byte, _ string, ns string, _ *authenticationv1.UserInfo, dc dynamic.Interface, _ k8s.Interface) ([]byte, error) {
	return dca_ac.Mutate(rawPod, ns, injectAgentSidecar, dc)
}

func injectAgentSidecar(pod *corev1.Pod, _ string, _ dynamic.Interface) error {
	if pod == nil {
		return errors.New("can't inject agent sidecar into nil pod")
	}

	for _, container := range pod.Spec.Containers {
		if container.Name == agentSidecarContainerName {
			log.Info("skipping agent sidecar injection: agent sidecar already exists")
			return nil
		}
	}

	agentSidecarContainer := getDefaultSidecarTemplate()

	err := applyProviderOverrides(agentSidecarContainer)
	if err != nil {
		return err
	}

	// User-provided overrides should always be applied last in order to have highest override-priority
	err = applyProfileOverrides(agentSidecarContainer)
	if err != nil {
		return err
	}

	pod.Spec.Containers = append(pod.Spec.Containers, *agentSidecarContainer)
	return nil
}

func getDefaultSidecarTemplate() *corev1.Container {
	ddSite := os.Getenv("DD_SITE")
	if ddSite == "" {
		ddSite = config.DefaultSite
	}

	containerRegistry := config.Datadog.GetString("admission_controller.agent_sidecar.container_registry")
	imageName := config.Datadog.GetString("admission_controller.agent_sidecar.image_name")
	imageTag := config.Datadog.GetString("admission_controller.agent_sidecar.image_tag")

	agentContainer := &corev1.Container{
		Env: []corev1.EnvVar{
			{
				Name: "DD_API_KEY",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						Key: "api-key",
						LocalObjectReference: corev1.LocalObjectReference{
							Name: "datadog-secret",
						},
					},
				},
			},
			{
				Name:  "DD_SITE",
				Value: ddSite,
			},
			{
				Name:  "DD_CLUSTER_NAME",
				Value: config.Datadog.GetString("cluster_name"),
			},
			{
				Name: "DD_KUBERNETES_KUBELET_NODENAME",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{
						APIVersion: "v1",
						FieldPath:  "spec.nodeName",
					},
				},
			},
		},
		Image:           fmt.Sprintf("%s/%s:%s", containerRegistry, imageName, imageTag),
		ImagePullPolicy: corev1.PullIfNotPresent,
		Name:            agentSidecarContainerName,
		Resources: corev1.ResourceRequirements{
			Requests: map[corev1.ResourceName]resource.Quantity{
				"memory": resource.MustParse("256Mi"),
				"cpu":    resource.MustParse("200m"),
			},
			Limits: map[corev1.ResourceName]resource.Quantity{
				"memory": resource.MustParse("256Mi"),
				"cpu":    resource.MustParse("200m"),
			},
		},
	}

	clusterAgentEnabled := config.Datadog.GetBool("admission_controller.agent_sidecar.cluster_agent.enabled")

	if clusterAgentEnabled {
		clusterAgentCmdPort := config.Datadog.GetInt("cluster_agent.cmd_port")
		clusterAgentServiceName := config.Datadog.GetString("cluster_agent.kubernetes_service_name")

		_ = withEnvOverrides(agentContainer, corev1.EnvVar{
			Name:  "DD_CLUSTER_AGENT_ENABLED",
			Value: "true",
		}, corev1.EnvVar{
			Name: "DD_CLUSTER_AGENT_AUTH_TOKEN",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					Key: "token",
					LocalObjectReference: corev1.LocalObjectReference{
						Name: "datadog-secret",
					},
				},
			},
		}, corev1.EnvVar{
			Name:  "DD_CLUSTER_AGENT_URL",
			Value: fmt.Sprintf("https://%s.%s.svc.cluster.local:%v", clusterAgentServiceName, apiCommon.GetMyNamespace(), clusterAgentCmdPort),
		}, corev1.EnvVar{
			Name:  "DD_ORCHESTRATOR_EXPLORER_ENABLED",
			Value: "true",
		})
	}

	return agentContainer
}