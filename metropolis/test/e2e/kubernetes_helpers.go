// Copyright 2020 The Monogon Project Authors.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
)

// makeTestDeploymentSpec generates a Deployment spec for a single pod running
// NGINX with a readiness probe. This allows verifying that the control plane
// is capable of scheduling simple pods and that kubelet works, its runtime is
// set up well enough to run a simple container and the network to the pod can
// pass readiness probe traffic.
func makeTestDeploymentSpec(name string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{
				"name": name,
			}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"name": name,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            "test",
							ImagePullPolicy: corev1.PullNever,
							Image:           "bazel/metropolis/test/e2e/preseedtest:preseedtest_image",
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromInt(80)},
								},
							},
						},
					},
				},
			},
		},
	}
}

// makeSelftestSpec generates a Job spec for the E2E self-test image.
func makeSelftestSpec(name string) *batchv1.Job {
	one := int32(1)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: batchv1.JobSpec{
			BackoffLimit: &one,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"job-name": name,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            "test",
							ImagePullPolicy: corev1.PullNever,
							Image:           "bazel/metropolis/test/e2e/selftest:selftest_image",
						},
					},
					RestartPolicy: corev1.RestartPolicyOnFailure,
				},
			},
		},
	}
}

// makeTestStatefulSet generates a StatefulSet spec
func makeTestStatefulSet(name string, volumeMode corev1.PersistentVolumeMode) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: appsv1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{
				"name": name,
			}},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "www"},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						Resources: corev1.ResourceRequirements{
							Requests: map[corev1.ResourceName]resource.Quantity{corev1.ResourceStorage: resource.MustParse("50Mi")},
						},
						VolumeMode: &volumeMode,
					},
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"name": name,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            "test",
							ImagePullPolicy: corev1.PullNever,
							Image:           "bazel/metropolis/test/e2e/preseedtest:preseedtest_image",
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromInt(80)},
								},
							},
						},
					},
				},
			},
		},
	}
}

func getPodLogLines(ctx context.Context, cs kubernetes.Interface, podName string, nlines int64) ([]string, error) {
	logsR := cs.CoreV1().Pods("default").GetLogs(podName, &corev1.PodLogOptions{
		TailLines: &nlines,
	})
	logs, err := logsR.Stream(ctx)
	if err != nil {
		return nil, fmt.Errorf("stream failed: %w", err)
	}
	var buf bytes.Buffer
	_, err = io.Copy(&buf, logs)
	if err != nil {
		return nil, fmt.Errorf("copy failed: %w", err)
	}
	lineStr := strings.Trim(buf.String(), "\n")
	lines := strings.Split(lineStr, "\n")
	lines = lines[len(lines)-int(nlines):]
	return lines, nil
}
