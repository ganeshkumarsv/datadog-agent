// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build orchestrator

package k8s

import (
	"sort"
	"testing"
	"time"

	model "github.com/DataDog/agent-payload/v5/process"
	"github.com/DataDog/datadog-agent/pkg/collector/corechecks/cluster/orchestrator/processors"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestExtractStatefulSet(t *testing.T) {
	timestamp := metav1.NewTime(time.Date(2014, time.January, 15, 0, 0, 0, 0, time.UTC)) // 1389744000
	testInt32 := int32(2)
	tests := map[string]struct {
		input             appsv1.StatefulSet
		labelsAsTags      map[string]string
		annotationsAsTags map[string]string
		expected          model.StatefulSet
	}{
		"full sts": {
			input: appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					UID:               types.UID("e42e5adc-0749-11e8-a2b8-000c29dea4f6"),
					Name:              "sts",
					Namespace:         "namespace",
					CreationTimestamp: timestamp,
					Labels: map[string]string{
						"label": "foo",
					},
					Annotations: map[string]string{
						"annotation": "bar",
					},
					ResourceVersion: "1234",
				},
				Spec: appsv1.StatefulSetSpec{
					Replicas:             &testInt32,
					RevisionHistoryLimit: &testInt32,
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": "test-sts",
						},
					},
					UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
						Type: appsv1.StatefulSetUpdateStrategyType("RollingUpdate"),
						RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{
							Partition: &testInt32,
						},
					},
				},
				Status: appsv1.StatefulSetStatus{
					Conditions: []appsv1.StatefulSetCondition{
						{
							Type:               "Test",
							Status:             v1.ConditionFalse,
							LastTransitionTime: timestamp,
							Reason:             "testing",
							Message:            "123",
						},
					},
					ObservedGeneration: 3,
					ReadyReplicas:      2,
					Replicas:           2,
					UpdatedReplicas:    2,
				},
			},
			labelsAsTags: map[string]string{
				"label": "application",
			},
			annotationsAsTags: map[string]string{
				"annotation": "annotation_key",
			},
			expected: model.StatefulSet{
				Metadata: &model.Metadata{
					Name:              "sts",
					Namespace:         "namespace",
					Uid:               "e42e5adc-0749-11e8-a2b8-000c29dea4f6",
					CreationTimestamp: 1389744000,
					Labels:            []string{"label:foo"},
					Annotations:       []string{"annotation:bar"},
					ResourceVersion:   "1234",
				},
				Conditions: []*model.StatefulSetCondition{
					{
						Type:               "Test",
						Status:             string(v1.ConditionFalse),
						LastTransitionTime: timestamp.Unix(),
						Reason:             "testing",
						Message:            "123",
					},
				},
				Tags: []string{
					"kube_condition_test:false",
					"application:foo",
					"annotation_key:bar",
				},
				Spec: &model.StatefulSetSpec{
					DesiredReplicas: 2,
					UpdateStrategy:  "RollingUpdate",
					Partition:       2,
					Selectors: []*model.LabelSelectorRequirement{
						{
							Key:      "app",
							Operator: "In",
							Values:   []string{"test-sts"},
						},
					},
				},
				Status: &model.StatefulSetStatus{
					Replicas:        2,
					ReadyReplicas:   2,
					UpdatedReplicas: 2,
				},
			},
		},
		"empty sts": {input: appsv1.StatefulSet{}, expected: model.StatefulSet{Metadata: &model.Metadata{}, Spec: &model.StatefulSetSpec{}, Status: &model.StatefulSetStatus{}}},
		"sts with resources": {
			input: appsv1.StatefulSet{
				Spec: appsv1.StatefulSetSpec{
					Template: getTemplateWithResourceRequirements(),
				},
			}, expected: model.StatefulSet{
				Metadata: &model.Metadata{},
				Spec:     &model.StatefulSetSpec{ResourceRequirements: getExpectedModelResourceRequirements()},
				Status:   &model.StatefulSetStatus{}}},
		"partial sts": {
			input: appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sts",
					Namespace: "namespace",
				},
				Spec: appsv1.StatefulSetSpec{
					ServiceName: "service sts",
				},
			}, expected: model.StatefulSet{
				Metadata: &model.Metadata{
					Name:      "sts",
					Namespace: "namespace",
				},
				Spec: &model.StatefulSetSpec{
					ServiceName: "service sts",
				},
				Status: &model.StatefulSetStatus{},
			},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			pctx := &processors.K8sProcessorContext{
				LabelsAsTags:      tc.labelsAsTags,
				AnnotationsAsTags: tc.annotationsAsTags,
			}
			actual := ExtractStatefulSet(pctx, &tc.input)
			sort.Strings(actual.Tags)
			sort.Strings(tc.expected.Tags)
			assert.Equal(t, &tc.expected, actual)
		})
	}
}
