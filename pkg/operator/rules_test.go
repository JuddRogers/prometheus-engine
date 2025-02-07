// Copyright 2023 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package operator

import (
	"testing"

	monitoringv1 "github.com/GoogleCloudPlatform/prometheus-engine/pkg/operator/apis/monitoring/v1"
	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestGenerateRules(t *testing.T) {
	var wantRules = `groups:
    - name: test-group
      rules:
        - record: test_record
          expr: test_expr{cluster="test-cluster",location="us-central1",namespace="test-namespace",project_id="123"}
          labels:
            cluster: test-cluster
            location: us-central1
            namespace: test-namespace
            project_id: "123"
`

	tests := []struct {
		name        string
		apiRules    *monitoringv1.Rules
		projectID   string
		location    string
		clusterName string
		want        string
		wantErr     bool
	}{
		{
			name: "good rules",
			apiRules: &monitoringv1.Rules{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "test-namespace",
				},
				Spec: monitoringv1.RulesSpec{
					Groups: []monitoringv1.RuleGroup{
						{
							Name: "test-group",
							Rules: []monitoringv1.Rule{
								{
									Record: "test_record",
									Expr:   "test_expr",
								},
							},
						},
					},
				},
			},
			projectID:   "123",
			location:    "us-central1",
			clusterName: "test-cluster",
			want:        wantRules,
			wantErr:     false,
		},
		{
			name: "invalid rules",
			apiRules: &monitoringv1.Rules{
				Spec: monitoringv1.RulesSpec{
					Groups: []monitoringv1.RuleGroup{
						{
							Name: "test-group",
							Rules: []monitoringv1.Rule{
								{
									Record: "test_record",
									Expr:   "test_expr{",
								},
							},
						},
					},
				},
			},
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := generateRules(test.apiRules, test.projectID, test.location, test.clusterName)
			if (err == nil && test.wantErr) || (err != nil && !test.wantErr) {
				t.Fatalf("expected err: %v; actual %v", test.wantErr, err)
			}
			if string(got) != test.want {
				t.Errorf("expected: %v; actual %v", test.want, string(got))
			}
		})
	}
}

func TestGenerateClusterRules(t *testing.T) {
	var wantClusterRules = `groups:
    - name: test-group
      rules:
        - record: test_record
          expr: test_expr{cluster="test-cluster",location="us-central1",project_id="123"}
          labels:
            cluster: test-cluster
            location: us-central1
            project_id: "123"
`

	tests := []struct {
		name        string
		apiRules    *monitoringv1.ClusterRules
		projectID   string
		location    string
		clusterName string
		want        string
		wantErr     bool
	}{
		{
			name: "good cluster rules",
			apiRules: &monitoringv1.ClusterRules{
				Spec: monitoringv1.RulesSpec{
					Groups: []monitoringv1.RuleGroup{
						{
							Name: "test-group",
							Rules: []monitoringv1.Rule{
								{
									Record: "test_record",
									Expr:   "test_expr",
								},
							},
						},
					},
				},
			},
			projectID:   "123",
			location:    "us-central1",
			clusterName: "test-cluster",
			want:        wantClusterRules,
			wantErr:     false,
		},
		{
			name: "invalid cluster rules",
			apiRules: &monitoringv1.ClusterRules{
				Spec: monitoringv1.RulesSpec{
					Groups: []monitoringv1.RuleGroup{
						{
							Name: "test-group",
							Rules: []monitoringv1.Rule{
								{
									Record: "test_record",
									Expr:   "test_expr{",
								},
							},
						},
					},
				},
			},
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := generateClusterRules(test.apiRules, test.projectID, test.location, test.clusterName)
			if (err == nil && test.wantErr) || (err != nil && !test.wantErr) {
				t.Fatalf("expected err: %v; actual %v", test.wantErr, err)
			}
			if diff := cmp.Diff(test.want, string(got)); diff != "" {
				t.Fatalf("unexpected result (-want, +got):\n %s", diff)
			}
		})
	}
}

func TestGenerateGlobalRules(t *testing.T) {
	var wantGlobalRules = `groups:
    - name: test-group
      rules:
        - record: test_record
          expr: test_expr
`

	tests := []struct {
		name     string
		apiRules *monitoringv1.GlobalRules
		want     string
		wantErr  bool
	}{
		{
			name: "good global rules",
			apiRules: &monitoringv1.GlobalRules{
				Spec: monitoringv1.RulesSpec{
					Groups: []monitoringv1.RuleGroup{
						{
							Name: "test-group",
							Rules: []monitoringv1.Rule{
								{
									Record: "test_record",
									Expr:   "test_expr",
								},
							},
						},
					},
				},
			},
			want:    wantGlobalRules,
			wantErr: false,
		},
		{
			name: "invalid global rules",
			apiRules: &monitoringv1.GlobalRules{
				Spec: monitoringv1.RulesSpec{
					Groups: []monitoringv1.RuleGroup{
						{
							Name: "test-group",
							Rules: []monitoringv1.Rule{
								{
									Record: "test_record",
									Expr:   "test_expr{",
								},
							},
						},
					},
				},
			},
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := generateGlobalRules(test.apiRules)
			if (err == nil && test.wantErr) || (err != nil && !test.wantErr) {
				t.Fatalf("expected err: %v; actual %v", test.wantErr, err)
			}
			if diff := cmp.Diff(test.want, string(got)); diff != "" {
				t.Fatalf("unexpected result (-want, +got):\n %s", diff)
			}
		})
	}
}
