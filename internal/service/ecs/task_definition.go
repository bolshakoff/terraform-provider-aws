// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package ecs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/YakDriver/regexache"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/arn"
	"github.com/aws/aws-sdk-go/private/protocol/json/jsonutil"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/structure"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
	"github.com/hashicorp/terraform-provider-aws/internal/conns"
	"github.com/hashicorp/terraform-provider-aws/internal/create"
	"github.com/hashicorp/terraform-provider-aws/internal/errs"
	"github.com/hashicorp/terraform-provider-aws/internal/errs/sdkdiag"
	"github.com/hashicorp/terraform-provider-aws/internal/flex"
	tftags "github.com/hashicorp/terraform-provider-aws/internal/tags"
	"github.com/hashicorp/terraform-provider-aws/internal/verify"
	"github.com/hashicorp/terraform-provider-aws/names"
)

// @SDKResource("aws_ecs_task_definition", name="Task Definition")
// @Tags(identifierAttribute="arn")
func ResourceTaskDefinition() *schema.Resource {
	//lintignore:R011
	return &schema.Resource{
		CreateWithoutTimeout: resourceTaskDefinitionCreate,
		ReadWithoutTimeout:   resourceTaskDefinitionRead,
		UpdateWithoutTimeout: resourceTaskDefinitionUpdate,
		DeleteWithoutTimeout: resourceTaskDefinitionDelete,

		Importer: &schema.ResourceImporter{
			StateContext: func(ctx context.Context, d *schema.ResourceData, meta interface{}) ([]*schema.ResourceData, error) {
				d.Set("arn", d.Id())

				idErr := fmt.Errorf("Expected ID in format of arn:PARTITION:ecs:REGION:ACCOUNTID:task-definition/FAMILY:REVISION and provided: %s", d.Id())
				resARN, err := arn.Parse(d.Id())
				if err != nil {
					return nil, idErr
				}
				familyRevision := strings.TrimPrefix(resARN.Resource, "task-definition/")
				familyRevisionParts := strings.Split(familyRevision, ":")
				if len(familyRevisionParts) != 2 {
					return nil, idErr
				}
				d.SetId(familyRevisionParts[0])

				return []*schema.ResourceData{d}, nil
			},
		},

		CustomizeDiff: verify.SetTagsDiff,

		SchemaVersion: 1,
		MigrateState:  resourceTaskDefinitionMigrateState,

		Schema: map[string]*schema.Schema{
			"arn": {
				Type:     schema.TypeString,
				Computed: true,
			},
			"arn_without_revision": {
				Type:     schema.TypeString,
				Computed: true,
			},
			"container_definitions": {
				Type: schema.TypeString,
				// TODO return to true
				Required: false,
				ForceNew: true,
				StateFunc: func(v interface{}) string {
					// Sort the lists of environment variables as they are serialized to state, so we won't get
					// spurious reorderings in plans (diff is suppressed if the environment variables haven't changed,
					// but they still show in the plan if some other property changes).
					orderedCDs, _ := expandContainerDefinitions(v.(string))
					containerDefinitions(orderedCDs).OrderEnvironmentVariables()
					containerDefinitions(orderedCDs).OrderSecrets()
					unnormalizedJson, _ := flattenContainerDefinitions(orderedCDs)
					json, _ := structure.NormalizeJsonString(unnormalizedJson)
					return json
				},
				DiffSuppressFunc: func(k, old, new string, d *schema.ResourceData) bool {
					networkMode, ok := d.GetOk("network_mode")
					isAWSVPC := ok && networkMode.(string) == ecs.NetworkModeAwsvpc
					equal, _ := ContainerDefinitionsAreEquivalent(old, new, isAWSVPC)
					return equal
				},
				ValidateFunc: ValidTaskDefinitionContainerDefinitions,
			},
			"container_definitions_structured": {
				Type:     schema.TypeList,
				Required: true,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						// Copied from GCP provider
						"image": {
							Type:        schema.TypeString,
							Required:    true,
							Description: `URL of the Container image in Google Container Registry or Google Artifact Registry. More info: https://kubernetes.io/docs/concepts/containers/images`,
						},
						"args": {
							Type:        schema.TypeList,
							Optional:    true,
							Description: `Arguments to the entrypoint. The docker image's CMD is used if this is not provided. Variable references $(VAR_NAME) are expanded using the container's environment. If a variable cannot be resolved, the reference in the input string will be unchanged. The $(VAR_NAME) syntax can be escaped with a double $$, ie: $$(VAR_NAME). Escaped references will never be expanded, regardless of whether the variable exists or not. More info: https://kubernetes.io/docs/tasks/inject-data-application/define-command-argument-container/#running-a-command-in-a-shell`,
							Elem: &schema.Schema{
								Type: schema.TypeString,
							},
						},
						"command": {
							Type:        schema.TypeList,
							Optional:    true,
							Description: `Entrypoint array. Not executed within a shell. The docker image's ENTRYPOINT is used if this is not provided. Variable references $(VAR_NAME) are expanded using the container's environment. If a variable cannot be resolved, the reference in the input string will be unchanged. The $(VAR_NAME) syntax can be escaped with a double $$, ie: $$(VAR_NAME). Escaped references will never be expanded, regardless of whether the variable exists or not. More info: https://kubernetes.io/docs/tasks/inject-data-application/define-command-argument-container/#running-a-command-in-a-shell`,
							Elem: &schema.Schema{
								Type: schema.TypeString,
							},
						},
						"depends_on": {
							Type:        schema.TypeList,
							Optional:    true,
							Description: `Containers which should be started before this container. If specified the container will wait to start until all containers with the listed names are healthy.`,
							Elem: &schema.Schema{
								Type: schema.TypeString,
							},
						},
						"env": {
							Type:        schema.TypeList,
							Optional:    true,
							Description: `List of environment variables to set in the container.`,
							Elem: &schema.Resource{
								Schema: map[string]*schema.Schema{
									"name": {
										Type:        schema.TypeString,
										Required:    true,
										Description: `Name of the environment variable. Must be a C_IDENTIFIER, and mnay not exceed 32768 characters.`,
									},
									"value": {
										Type:        schema.TypeString,
										Optional:    true,
										Description: `Variable references $(VAR_NAME) are expanded using the previous defined environment variables in the container and any route environment variables. If a variable cannot be resolved, the reference in the input string will be unchanged. The $(VAR_NAME) syntax can be escaped with a double $$, ie: $$(VAR_NAME). Escaped references will never be expanded, regardless of whether the variable exists or not. Defaults to "", and the maximum length is 32768 bytes`,
									},
									"value_source": {
										Type:        schema.TypeList,
										Optional:    true,
										Description: `Source for the environment variable's value.`,
										MaxItems:    1,
										Elem: &schema.Resource{
											Schema: map[string]*schema.Schema{
												"secret_key_ref": {
													Type:        schema.TypeList,
													Optional:    true,
													Description: `Selects a secret and a specific version from Cloud Secret Manager.`,
													MaxItems:    1,
													Elem: &schema.Resource{
														Schema: map[string]*schema.Schema{
															"secret": {
																Type:        schema.TypeString,
																Required:    true,
																Description: `The name of the secret in Cloud Secret Manager. Format: {secretName} if the secret is in the same project. projects/{project}/secrets/{secretName} if the secret is in a different project.`,
															},
															"version": {
																Type:        schema.TypeString,
																Optional:    true,
																Description: `The Cloud Secret Manager secret version. Can be 'latest' for the latest value or an integer for a specific version.`,
															},
														},
													},
												},
											},
										},
									},
								},
							},
						},
						"liveness_probe": {
							Type:        schema.TypeList,
							Computed:    true,
							Optional:    true,
							Description: `Periodic probe of container liveness. Container will be restarted if the probe fails. More info: https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle#container-probes`,
							MaxItems:    1,
							Elem: &schema.Resource{
								Schema: map[string]*schema.Schema{
									"failure_threshold": {
										Type:        schema.TypeInt,
										Optional:    true,
										Description: `Minimum consecutive failures for the probe to be considered failed after having succeeded. Defaults to 3. Minimum value is 1.`,
										Default:     3,
									},
									"grpc": {
										Type:        schema.TypeList,
										Optional:    true,
										Description: `GRPC specifies an action involving a GRPC port.`,
										MaxItems:    1,
										Elem: &schema.Resource{
											Schema: map[string]*schema.Schema{
												"port": {
													Type:     schema.TypeInt,
													Computed: true,
													Optional: true,
													Description: `Port number to access on the container. Number must be in the range 1 to 65535.
If not specified, defaults to the same value as container.ports[0].containerPort.`,
												},
												"service": {
													Type:     schema.TypeString,
													Optional: true,
													Description: `The name of the service to place in the gRPC HealthCheckRequest
(see https://github.com/grpc/grpc/blob/master/doc/health-checking.md).
If this is not specified, the default behavior is defined by gRPC.`,
												},
											},
										},
									},
									"http_get": {
										Type:        schema.TypeList,
										Optional:    true,
										Description: `HTTPGet specifies the http request to perform.`,
										MaxItems:    1,
										Elem: &schema.Resource{
											Schema: map[string]*schema.Schema{
												"http_headers": {
													Type:        schema.TypeList,
													Optional:    true,
													Description: `Custom headers to set in the request. HTTP allows repeated headers.`,
													Elem: &schema.Resource{
														Schema: map[string]*schema.Schema{
															"name": {
																Type:        schema.TypeString,
																Required:    true,
																Description: `The header field name`,
															},
															"value": {
																Type:        schema.TypeString,
																Optional:    true,
																Description: `The header field value`,
																Default:     "",
															},
														},
													},
												},
												"path": {
													Type:        schema.TypeString,
													Optional:    true,
													Description: `Path to access on the HTTP server. Defaults to '/'.`,
													Default:     "/",
												},
												"port": {
													Type:     schema.TypeInt,
													Computed: true,
													Optional: true,
													Description: `Port number to access on the container. Number must be in the range 1 to 65535.
If not specified, defaults to the same value as container.ports[0].containerPort.`,
												},
											},
										},
									},
									"initial_delay_seconds": {
										Type:        schema.TypeInt,
										Optional:    true,
										Description: `Number of seconds after the container has started before the probe is initiated. Defaults to 0 seconds. Minimum value is 0. Maximum value for liveness probe is 3600. Maximum value for startup probe is 240. More info: https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle#container-probes`,
										Default:     0,
									},
									"period_seconds": {
										Type:        schema.TypeInt,
										Optional:    true,
										Description: `How often (in seconds) to perform the probe. Default to 10 seconds. Minimum value is 1. Maximum value for liveness probe is 3600. Maximum value for startup probe is 240. Must be greater or equal than timeoutSeconds`,
										Default:     10,
									},
									"tcp_socket": {
										Type:        schema.TypeList,
										Optional:    true,
										Description: `TCPSocketAction describes an action based on opening a socket`,
										MaxItems:    1,
										Elem: &schema.Resource{
											Schema: map[string]*schema.Schema{
												"port": {
													Type:     schema.TypeInt,
													Required: true,
													Description: `Port number to access on the container. Must be in the range 1 to 65535.
If not specified, defaults to the exposed port of the container, which
is the value of container.ports[0].containerPort.`,
												},
											},
										},
									},
									"timeout_seconds": {
										Type:        schema.TypeInt,
										Optional:    true,
										Description: `Number of seconds after which the probe times out. Defaults to 1 second. Minimum value is 1. Maximum value is 3600. Must be smaller than periodSeconds. More info: https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle#container-probes`,
										Default:     1,
									},
								},
							},
						},
						"name": {
							Type:        schema.TypeString,
							Optional:    true,
							Description: `Name of the container specified as a DNS_LABEL.`,
						},
						"ports": {
							Type:     schema.TypeList,
							Computed: true,
							Optional: true,
							Description: `List of ports to expose from the container. Only a single port can be specified. The specified ports must be listening on all interfaces (0.0.0.0) within the container to be accessible.

If omitted, a port number will be chosen and passed to the container through the PORT environment variable for the container to listen on`,
							MaxItems: 1,
							Elem: &schema.Resource{
								Schema: map[string]*schema.Schema{
									"container_port": {
										Type:        schema.TypeInt,
										Optional:    true,
										Description: `Port number the container listens on. This must be a valid TCP port number, 0 < containerPort < 65536.`,
									},
									"name": {
										Type:        schema.TypeString,
										Computed:    true,
										Optional:    true,
										Description: `If specified, used to specify which protocol to use. Allowed values are "http1" and "h2c".`,
									},
								},
							},
						},
						"resources": {
							Type:        schema.TypeList,
							Computed:    true,
							Optional:    true,
							Description: `Compute Resource requirements by this container. More info: https://kubernetes.io/docs/concepts/storage/persistent-volumes#resources`,
							MaxItems:    1,
							Elem: &schema.Resource{
								Schema: map[string]*schema.Schema{
									"cpu_idle": {
										Type:     schema.TypeBool,
										Optional: true,
										Description: `Determines whether CPU is only allocated during requests. True by default if the parent 'resources' field is not set. However, if
'resources' is set, this field must be explicitly set to true to preserve the default behavior.`,
									},
									"limits": {
										Type:        schema.TypeMap,
										Computed:    true,
										Optional:    true,
										Description: `Only memory and CPU are supported. Use key 'cpu' for CPU limit and 'memory' for memory limit. Note: The only supported values for CPU are '1', '2', '4', and '8'. Setting 4 CPU requires at least 2Gi of memory. The values of the map is string form of the 'quantity' k8s type: https://github.com/kubernetes/kubernetes/blob/master/staging/src/k8s.io/apimachinery/pkg/api/resource/quantity.go`,
										Elem:        &schema.Schema{Type: schema.TypeString},
									},
									"startup_cpu_boost": {
										Type:        schema.TypeBool,
										Optional:    true,
										Description: `Determines whether CPU should be boosted on startup of a new container instance above the requested CPU threshold, this can help reduce cold-start latency.`,
									},
								},
							},
						},
						"startup_probe": {
							Type:        schema.TypeList,
							Computed:    true,
							Optional:    true,
							Description: `Startup probe of application within the container. All other probes are disabled if a startup probe is provided, until it succeeds. Container will not be added to service endpoints if the probe fails. More info: https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle#container-probes`,
							MaxItems:    1,
							Elem: &schema.Resource{
								Schema: map[string]*schema.Schema{
									"failure_threshold": {
										Type:        schema.TypeInt,
										Optional:    true,
										Description: `Minimum consecutive failures for the probe to be considered failed after having succeeded. Defaults to 3. Minimum value is 1.`,
										Default:     3,
									},
									"grpc": {
										Type:        schema.TypeList,
										Optional:    true,
										Description: `GRPC specifies an action involving a GRPC port.`,
										MaxItems:    1,
										Elem: &schema.Resource{
											Schema: map[string]*schema.Schema{
												"port": {
													Type:     schema.TypeInt,
													Computed: true,
													Optional: true,
													Description: `Port number to access on the container. Number must be in the range 1 to 65535.
If not specified, defaults to the same value as container.ports[0].containerPort.`,
												},
												"service": {
													Type:     schema.TypeString,
													Optional: true,
													Description: `The name of the service to place in the gRPC HealthCheckRequest
(see https://github.com/grpc/grpc/blob/master/doc/health-checking.md).
If this is not specified, the default behavior is defined by gRPC.`,
												},
											},
										},
									},
									"http_get": {
										Type:        schema.TypeList,
										Optional:    true,
										Description: `HTTPGet specifies the http request to perform. Exactly one of HTTPGet or TCPSocket must be specified.`,
										MaxItems:    1,
										Elem: &schema.Resource{
											Schema: map[string]*schema.Schema{
												"http_headers": {
													Type:        schema.TypeList,
													Optional:    true,
													Description: `Custom headers to set in the request. HTTP allows repeated headers.`,
													Elem: &schema.Resource{
														Schema: map[string]*schema.Schema{
															"name": {
																Type:        schema.TypeString,
																Required:    true,
																Description: `The header field name`,
															},
															"value": {
																Type:        schema.TypeString,
																Optional:    true,
																Description: `The header field value`,
																Default:     "",
															},
														},
													},
												},
												"path": {
													Type:        schema.TypeString,
													Optional:    true,
													Description: `Path to access on the HTTP server. Defaults to '/'.`,
													Default:     "/",
												},
												"port": {
													Type:     schema.TypeInt,
													Computed: true,
													Optional: true,
													Description: `Port number to access on the container. Must be in the range 1 to 65535.
If not specified, defaults to the same value as container.ports[0].containerPort.`,
												},
											},
										},
									},
									"initial_delay_seconds": {
										Type:        schema.TypeInt,
										Optional:    true,
										Description: `Number of seconds after the container has started before the probe is initiated. Defaults to 0 seconds. Minimum value is 0. Maximum value for liveness probe is 3600. Maximum value for startup probe is 240. More info: https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle#container-probes`,
										Default:     0,
									},
									"period_seconds": {
										Type:        schema.TypeInt,
										Optional:    true,
										Description: `How often (in seconds) to perform the probe. Default to 10 seconds. Minimum value is 1. Maximum value for liveness probe is 3600. Maximum value for startup probe is 240. Must be greater or equal than timeoutSeconds`,
										Default:     10,
									},
									"tcp_socket": {
										Type:        schema.TypeList,
										Optional:    true,
										Description: `TCPSocket specifies an action involving a TCP port. Exactly one of HTTPGet or TCPSocket must be specified.`,
										MaxItems:    1,
										Elem: &schema.Resource{
											Schema: map[string]*schema.Schema{
												"port": {
													Type:     schema.TypeInt,
													Computed: true,
													Optional: true,
													Description: `Port number to access on the container. Must be in the range 1 to 65535.
If not specified, defaults to the same value as container.ports[0].containerPort.`,
												},
											},
										},
									},
									"timeout_seconds": {
										Type:        schema.TypeInt,
										Optional:    true,
										Description: `Number of seconds after which the probe times out. Defaults to 1 second. Minimum value is 1. Maximum value is 3600. Must be smaller than periodSeconds. More info: https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle#container-probes`,
										Default:     1,
									},
								},
							},
						},
						"volume_mounts": {
							Type:        schema.TypeList,
							Optional:    true,
							Description: `Volume to mount into the container's filesystem.`,
							Elem: &schema.Resource{
								Schema: map[string]*schema.Schema{
									"mount_path": {
										Type:        schema.TypeString,
										Required:    true,
										Description: `Path within the container at which the volume should be mounted. Must not contain ':'. For Cloud SQL volumes, it can be left empty, or must otherwise be /cloudsql. All instances defined in the Volume will be available as /cloudsql/[instance]. For more information on Cloud SQL volumes, visit https://cloud.google.com/sql/docs/mysql/connect-run`,
									},
									"name": {
										Type:        schema.TypeString,
										Required:    true,
										Description: `This must match the Name of a Volume.`,
									},
								},
							},
						},
						"working_dir": {
							Type:        schema.TypeString,
							Optional:    true,
							Description: `Container's working directory. If not specified, the container runtime's default will be used, which might be configured in the container image.`,
						},
					},
				},
			},
			"cpu": {
				Type:     schema.TypeString,
				Optional: true,
				ForceNew: true,
			},
			"ephemeral_storage": {
				Type:     schema.TypeList,
				MaxItems: 1,
				Optional: true,
				ForceNew: true,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"size_in_gib": {
							Type:         schema.TypeInt,
							Required:     true,
							ForceNew:     true,
							ValidateFunc: validation.IntBetween(21, 200),
						},
					},
				},
			},
			"execution_role_arn": {
				Type:         schema.TypeString,
				Optional:     true,
				ForceNew:     true,
				ValidateFunc: verify.ValidARN,
			},
			"family": {
				Type:     schema.TypeString,
				Required: true,
				ForceNew: true,
				ValidateFunc: validation.All(
					validation.StringLenBetween(1, 255),
					validation.StringMatch(regexache.MustCompile("^[0-9A-Za-z_-]+$"), "see https://docs.aws.amazon.com/AmazonECS/latest/APIReference/API_TaskDefinition.html"),
				),
			},
			"inference_accelerator": {
				Type:     schema.TypeSet,
				Optional: true,
				ForceNew: true,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"device_name": {
							Type:     schema.TypeString,
							Required: true,
							ForceNew: true,
						},
						"device_type": {
							Type:     schema.TypeString,
							Required: true,
							ForceNew: true,
						},
					},
				},
			},
			"ipc_mode": {
				Type:         schema.TypeString,
				Optional:     true,
				ForceNew:     true,
				ValidateFunc: validation.StringInSlice(ecs.IpcMode_Values(), false),
			},
			"memory": {
				Type:     schema.TypeString,
				Optional: true,
				ForceNew: true,
			},
			"network_mode": {
				Type:         schema.TypeString,
				Optional:     true,
				Computed:     true,
				ForceNew:     true,
				ValidateFunc: validation.StringInSlice(ecs.NetworkMode_Values(), false),
			},
			"pid_mode": {
				Type:         schema.TypeString,
				Optional:     true,
				ForceNew:     true,
				ValidateFunc: validation.StringInSlice(ecs.PidMode_Values(), false),
			},
			"placement_constraints": {
				Type:     schema.TypeSet,
				Optional: true,
				ForceNew: true,
				MaxItems: 10,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"expression": {
							Type:     schema.TypeString,
							ForceNew: true,
							Optional: true,
						},
						"type": {
							Type:         schema.TypeString,
							ForceNew:     true,
							Required:     true,
							ValidateFunc: validation.StringInSlice(ecs.TaskDefinitionPlacementConstraintType_Values(), false),
						},
					},
				},
			},
			"proxy_configuration": {
				Type:     schema.TypeList,
				MaxItems: 1,
				Optional: true,
				ForceNew: true,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"container_name": {
							Type:     schema.TypeString,
							Required: true,
							ForceNew: true,
						},
						"properties": {
							Type:     schema.TypeMap,
							Elem:     &schema.Schema{Type: schema.TypeString},
							Optional: true,
							ForceNew: true,
						},
						"type": {
							Type:         schema.TypeString,
							Default:      ecs.ProxyConfigurationTypeAppmesh,
							Optional:     true,
							ForceNew:     true,
							ValidateFunc: validation.StringInSlice(ecs.ProxyConfigurationType_Values(), false),
						},
					},
				},
			},
			"requires_compatibilities": {
				Type:     schema.TypeSet,
				Optional: true,
				ForceNew: true,
				Elem: &schema.Schema{
					Type: schema.TypeString,
					ValidateFunc: validation.StringInSlice([]string{
						"EC2",
						"FARGATE",
						"EXTERNAL",
					}, false),
				},
			},
			"revision": {
				Type:     schema.TypeInt,
				Computed: true,
			},
			"runtime_platform": {
				Type:     schema.TypeList,
				MaxItems: 1,
				Optional: true,
				ForceNew: true,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"cpu_architecture": {
							Type:         schema.TypeString,
							Optional:     true,
							ForceNew:     true,
							ValidateFunc: validation.StringInSlice(ecs.CPUArchitecture_Values(), false),
						},
						"operating_system_family": {
							Type:         schema.TypeString,
							Optional:     true,
							ForceNew:     true,
							ValidateFunc: validation.StringInSlice(ecs.OSFamily_Values(), false),
						},
					},
				},
			},
			"skip_destroy": {
				Type:     schema.TypeBool,
				Default:  false,
				Optional: true,
			},
			names.AttrTags:    tftags.TagsSchema(),
			names.AttrTagsAll: tftags.TagsSchemaComputed(),
			"task_role_arn": {
				Type:         schema.TypeString,
				Optional:     true,
				ForceNew:     true,
				ValidateFunc: verify.ValidARN,
			},
			"track_latest": {
				Type:     schema.TypeBool,
				Default:  false,
				Optional: true,
			},
			"volume": {
				Type:     schema.TypeSet,
				Optional: true,
				ForceNew: true,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"docker_volume_configuration": {
							Type:     schema.TypeList,
							Optional: true,
							ForceNew: true,
							MaxItems: 1,
							Elem: &schema.Resource{
								Schema: map[string]*schema.Schema{
									"autoprovision": {
										Type:     schema.TypeBool,
										Optional: true,
										ForceNew: true,
										Default:  false,
									},
									"driver": {
										Type:     schema.TypeString,
										ForceNew: true,
										Optional: true,
									},
									"driver_opts": {
										Type:     schema.TypeMap,
										Elem:     &schema.Schema{Type: schema.TypeString},
										ForceNew: true,
										Optional: true,
									},
									"labels": {
										Type:     schema.TypeMap,
										Elem:     &schema.Schema{Type: schema.TypeString},
										ForceNew: true,
										Optional: true,
									},
									"scope": {
										Type:         schema.TypeString,
										Optional:     true,
										Computed:     true,
										ForceNew:     true,
										ValidateFunc: validation.StringInSlice(ecs.Scope_Values(), false),
									},
								},
							},
						},
						"efs_volume_configuration": {
							Type:     schema.TypeList,
							Optional: true,
							ForceNew: true,
							MaxItems: 1,
							Elem: &schema.Resource{
								Schema: map[string]*schema.Schema{
									"authorization_config": {
										Type:     schema.TypeList,
										Optional: true,
										ForceNew: true,
										MaxItems: 1,
										Elem: &schema.Resource{
											Schema: map[string]*schema.Schema{
												"access_point_id": {
													Type:     schema.TypeString,
													ForceNew: true,
													Optional: true,
												},
												"iam": {
													Type:         schema.TypeString,
													ForceNew:     true,
													Optional:     true,
													ValidateFunc: validation.StringInSlice(ecs.EFSAuthorizationConfigIAM_Values(), false),
												},
											},
										},
									},
									"file_system_id": {
										Type:     schema.TypeString,
										ForceNew: true,
										Required: true,
									},
									"root_directory": {
										Type:     schema.TypeString,
										ForceNew: true,
										Optional: true,
										Default:  "/",
									},
									"transit_encryption": {
										Type:         schema.TypeString,
										ForceNew:     true,
										Optional:     true,
										ValidateFunc: validation.StringInSlice(ecs.EFSTransitEncryption_Values(), false),
									},
									"transit_encryption_port": {
										Type:         schema.TypeInt,
										ForceNew:     true,
										Optional:     true,
										ValidateFunc: validation.IsPortNumberOrZero,
										Default:      0,
									},
								},
							},
						},
						"fsx_windows_file_server_volume_configuration": {
							Type:     schema.TypeList,
							Optional: true,
							ForceNew: true,
							MaxItems: 1,
							Elem: &schema.Resource{
								Schema: map[string]*schema.Schema{
									"authorization_config": {
										Type:     schema.TypeList,
										Required: true,
										ForceNew: true,
										MaxItems: 1,
										Elem: &schema.Resource{
											Schema: map[string]*schema.Schema{
												"credentials_parameter": {
													Type:         schema.TypeString,
													ForceNew:     true,
													Required:     true,
													ValidateFunc: verify.ValidARN,
												},
												"domain": {
													Type:     schema.TypeString,
													ForceNew: true,
													Required: true,
												},
											},
										},
									},
									"file_system_id": {
										Type:     schema.TypeString,
										ForceNew: true,
										Required: true,
									},
									"root_directory": {
										Type:     schema.TypeString,
										ForceNew: true,
										Required: true,
									},
								},
							},
						},
						"host_path": {
							Type:     schema.TypeString,
							Optional: true,
							ForceNew: true,
						},
						"name": {
							Type:     schema.TypeString,
							Required: true,
							ForceNew: true,
						},
					},
				},
				Set: resourceTaskDefinitionVolumeHash,
			},
		},
	}
}

func ValidTaskDefinitionContainerDefinitions(v interface{}, k string) (ws []string, errors []error) {
	value := v.(string)
	_, err := expandContainerDefinitions(value)
	if err != nil {
		errors = append(errors, fmt.Errorf("ECS Task Definition container_definitions is invalid: %s", err))
	}
	return
}

func resourceTaskDefinitionCreate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	var diags diag.Diagnostics
	conn := meta.(*conns.AWSClient).ECSConn(ctx)

	rawDefinitions := d.Get("container_definitions").(string)
	containerDefinitionsJSON, err := expandContainerDefinitions(rawDefinitions)
	if err != nil {
		// TODO improve error message
		return sdkdiag.AppendErrorf(diags, "creating ECS Task Definition (%s): %s", d.Get("family").(string), err)
	}

	// TODO: add logic for the precedence of formats

	v := d.Get("container_definitions_structured").([]interface{})
	containerDefinitionsStructured := expandContainerDefinitionsStructured(v)

	var containerDefinitions []*ecs.ContainerDefinition
	if containerDefinitionsStructured != nil {
		containerDefinitions = containerDefinitionsStructured
	} else {
		containerDefinitions = containerDefinitionsJSON
	}

	input := &ecs.RegisterTaskDefinitionInput{
		ContainerDefinitions: containerDefinitions,
		Family:               aws.String(d.Get("family").(string)),
		Tags:                 getTagsIn(ctx),
	}

	if v, ok := d.GetOk("cpu"); ok {
		input.Cpu = aws.String(v.(string))
	}

	if v, ok := d.GetOk("ephemeral_storage"); ok && len(v.([]interface{})) > 0 {
		input.EphemeralStorage = expandTaskDefinitionEphemeralStorage(v.([]interface{}))
	}

	if v, ok := d.GetOk("execution_role_arn"); ok {
		input.ExecutionRoleArn = aws.String(v.(string))
	}

	if v, ok := d.GetOk("inference_accelerator"); ok {
		input.InferenceAccelerators = expandInferenceAccelerators(v.(*schema.Set).List())
	}

	if v, ok := d.GetOk("ipc_mode"); ok {
		input.IpcMode = aws.String(v.(string))
	}

	if v, ok := d.GetOk("memory"); ok {
		input.Memory = aws.String(v.(string))
	}

	if v, ok := d.GetOk("network_mode"); ok {
		input.NetworkMode = aws.String(v.(string))
	}

	if v, ok := d.GetOk("pid_mode"); ok {
		input.PidMode = aws.String(v.(string))
	}

	if constraints := d.Get("placement_constraints").(*schema.Set).List(); len(constraints) > 0 {
		cons, err := expandTaskDefinitionPlacementConstraints(constraints)
		if err != nil {
			return sdkdiag.AppendErrorf(diags, "creating ECS Task Definition (%s): %s", d.Get("family").(string), err)
		}
		input.PlacementConstraints = cons
	}

	if proxyConfigs := d.Get("proxy_configuration").([]interface{}); len(proxyConfigs) > 0 {
		input.ProxyConfiguration = expandTaskDefinitionProxyConfiguration(proxyConfigs)
	}

	if v, ok := d.GetOk("requires_compatibilities"); ok && v.(*schema.Set).Len() > 0 {
		input.RequiresCompatibilities = flex.ExpandStringSet(v.(*schema.Set))
	}

	if runtimePlatformConfigs := d.Get("runtime_platform").([]interface{}); len(runtimePlatformConfigs) > 0 && runtimePlatformConfigs[0] != nil {
		input.RuntimePlatform = expandTaskDefinitionRuntimePlatformConfiguration(runtimePlatformConfigs)
	}

	if v, ok := d.GetOk("task_role_arn"); ok {
		input.TaskRoleArn = aws.String(v.(string))
	}

	if v, ok := d.GetOk("volume"); ok {
		volumes := expandVolumes(v.(*schema.Set).List())
		input.Volumes = volumes
	}

	output, err := conn.RegisterTaskDefinitionWithContext(ctx, input)

	// Some partitions (e.g. ISO) may not support tag-on-create.
	if input.Tags != nil && errs.IsUnsupportedOperationInPartitionError(conn.PartitionID, err) {
		input.Tags = nil

		output, err = conn.RegisterTaskDefinitionWithContext(ctx, input)
	}

	if err != nil {
		return sdkdiag.AppendErrorf(diags, "creating ECS Task Definition (%s): %s", d.Get("family").(string), err)
	}

	taskDefinition := *output.TaskDefinition // nosemgrep:ci.semgrep.aws.prefer-pointer-conversion-assignment // false positive

	d.SetId(aws.StringValue(taskDefinition.Family))
	d.Set("arn", taskDefinition.TaskDefinitionArn)
	d.Set("arn_without_revision", StripRevision(aws.StringValue(taskDefinition.TaskDefinitionArn)))

	// For partitions not supporting tag-on-create, attempt tag after create.
	if tags := getTagsIn(ctx); input.Tags == nil && len(tags) > 0 {
		err := createTags(ctx, conn, aws.StringValue(taskDefinition.TaskDefinitionArn), tags)

		// If default tags only, continue. Otherwise, error.
		if v, ok := d.GetOk(names.AttrTags); (!ok || len(v.(map[string]interface{})) == 0) && errs.IsUnsupportedOperationInPartitionError(conn.PartitionID, err) {
			return append(diags, resourceTaskDefinitionRead(ctx, d, meta)...)
		}

		if err != nil {
			return sdkdiag.AppendErrorf(diags, "setting ECS Task Definition (%s) tags: %s", d.Id(), err)
		}
	}

	return append(diags, resourceTaskDefinitionRead(ctx, d, meta)...)
}

func resourceTaskDefinitionRead(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	var diags diag.Diagnostics
	conn := meta.(*conns.AWSClient).ECSConn(ctx)

	trackedTaskDefinition := d.Get("arn").(string)
	if _, ok := d.GetOk("track_latest"); ok {
		trackedTaskDefinition = d.Get("family").(string)
	}

	input := ecs.DescribeTaskDefinitionInput{
		Include:        aws.StringSlice([]string{ecs.TaskDefinitionFieldTags}),
		TaskDefinition: aws.String(trackedTaskDefinition),
	}

	out, err := conn.DescribeTaskDefinitionWithContext(ctx, &input)

	// Some partitions (i.e., ISO) may not support tagging, giving error
	if errs.IsUnsupportedOperationInPartitionError(conn.PartitionID, err) {
		log.Printf("[WARN] ECS tagging failed describing Task Definition (%s) with tags: %s; retrying without tags", d.Id(), err)

		input.Include = nil
		out, err = conn.DescribeTaskDefinitionWithContext(ctx, &input)
	}

	if err != nil {
		return sdkdiag.AppendErrorf(diags, "reading ECS Task Definition (%s): %s", d.Id(), err)
	}

	log.Printf("[DEBUG] Received task definition %s, status:%s\n %s", aws.StringValue(out.TaskDefinition.Family),
		aws.StringValue(out.TaskDefinition.Status), out)

	taskDefinition := out.TaskDefinition

	if aws.StringValue(taskDefinition.Status) == ecs.TaskDefinitionStatusInactive {
		log.Printf("[DEBUG] Removing ECS task definition %s because it's INACTIVE", aws.StringValue(out.TaskDefinition.Family))
		d.SetId("")
		return diags
	}

	d.SetId(aws.StringValue(taskDefinition.Family))
	d.Set("arn", taskDefinition.TaskDefinitionArn)
	d.Set("arn_without_revision", StripRevision(aws.StringValue(taskDefinition.TaskDefinitionArn)))
	d.Set("family", taskDefinition.Family)
	d.Set("revision", taskDefinition.Revision)
	d.Set("track_latest", d.Get("track_latest"))

	// Sort the lists of environment variables as they come in, so we won't get spurious reorderings in plans
	// (diff is suppressed if the environment variables haven't changed, but they still show in the plan if
	// some other property changes).
	containerDefinitions(taskDefinition.ContainerDefinitions).OrderEnvironmentVariables()
	containerDefinitions(taskDefinition.ContainerDefinitions).OrderSecrets()

	defs, err := flattenContainerDefinitions(taskDefinition.ContainerDefinitions)
	if err != nil {
		return sdkdiag.AppendErrorf(diags, "reading ECS Task Definition (%s): %s", d.Id(), err)
	}
	err = d.Set("container_definitions", defs)
	if err != nil {
		return sdkdiag.AppendErrorf(diags, "reading ECS Task Definition (%s): %s", d.Id(), err)
	}

	d.Set("task_role_arn", taskDefinition.TaskRoleArn)
	d.Set("execution_role_arn", taskDefinition.ExecutionRoleArn)
	d.Set("cpu", taskDefinition.Cpu)
	d.Set("memory", taskDefinition.Memory)
	d.Set("network_mode", taskDefinition.NetworkMode)
	d.Set("ipc_mode", taskDefinition.IpcMode)
	d.Set("pid_mode", taskDefinition.PidMode)

	if err := d.Set("volume", flattenVolumes(taskDefinition.Volumes)); err != nil {
		return sdkdiag.AppendErrorf(diags, "setting volume: %s", err)
	}

	if err := d.Set("inference_accelerator", flattenInferenceAccelerators(taskDefinition.InferenceAccelerators)); err != nil {
		return sdkdiag.AppendErrorf(diags, "setting inference accelerators: %s", err)
	}

	if err := d.Set("placement_constraints", flattenPlacementConstraints(taskDefinition.PlacementConstraints)); err != nil {
		return sdkdiag.AppendErrorf(diags, "setting placement_constraints: %s", err)
	}

	if err := d.Set("requires_compatibilities", flex.FlattenStringList(taskDefinition.RequiresCompatibilities)); err != nil {
		return sdkdiag.AppendErrorf(diags, "setting requires_compatibilities: %s", err)
	}

	if err := d.Set("runtime_platform", flattenRuntimePlatform(taskDefinition.RuntimePlatform)); err != nil {
		return sdkdiag.AppendErrorf(diags, "setting runtime_platform: %s", err)
	}

	if err := d.Set("proxy_configuration", flattenProxyConfiguration(taskDefinition.ProxyConfiguration)); err != nil {
		return sdkdiag.AppendErrorf(diags, "setting proxy_configuration: %s", err)
	}

	if err := d.Set("ephemeral_storage", flattenTaskDefinitionEphemeralStorage(taskDefinition.EphemeralStorage)); err != nil {
		return sdkdiag.AppendErrorf(diags, "setting ephemeral_storage: %s", err)
	}

	setTagsOut(ctx, out.Tags)

	return diags
}

func resourceTaskDefinitionUpdate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	var diags diag.Diagnostics

	// Tags only.

	return append(diags, resourceTaskDefinitionRead(ctx, d, meta)...)
}

func resourceTaskDefinitionDelete(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	var diags diag.Diagnostics
	if v, ok := d.GetOk("skip_destroy"); ok && v.(bool) {
		log.Printf("[DEBUG] Retaining ECS Task Definition Revision %q", d.Id())
		return diags
	}

	conn := meta.(*conns.AWSClient).ECSConn(ctx)

	_, err := conn.DeregisterTaskDefinitionWithContext(ctx, &ecs.DeregisterTaskDefinitionInput{
		TaskDefinition: aws.String(d.Get("arn").(string)),
	})
	if err != nil {
		return sdkdiag.AppendErrorf(diags, "deleting ECS Task Definition (%s): %s", d.Id(), err)
	}

	return diags
}

func resourceTaskDefinitionVolumeHash(v interface{}) int {
	var buf bytes.Buffer
	m := v.(map[string]interface{})
	buf.WriteString(fmt.Sprintf("%s-", m["name"].(string)))
	buf.WriteString(fmt.Sprintf("%s-", m["host_path"].(string)))

	if v, ok := m["efs_volume_configuration"]; ok && len(v.([]interface{})) > 0 && v.([]interface{})[0] != nil {
		m := v.([]interface{})[0].(map[string]interface{})

		if v, ok := m["file_system_id"]; ok && v.(string) != "" {
			buf.WriteString(fmt.Sprintf("%s-", v.(string)))
		}

		if v, ok := m["root_directory"]; ok && v.(string) != "" {
			buf.WriteString(fmt.Sprintf("%s-", v.(string)))
		}

		if v, ok := m["transit_encryption"]; ok && v.(string) != "" {
			buf.WriteString(fmt.Sprintf("%s-", v.(string)))
		}
		if v, ok := m["transit_encryption_port"]; ok && v.(int) > 0 {
			buf.WriteString(fmt.Sprintf("%d-", v.(int)))
		}
		if v, ok := m["authorization_config"]; ok && len(v.([]interface{})) > 0 && v.([]interface{})[0] != nil {
			m := v.([]interface{})[0].(map[string]interface{})
			if v, ok := m["access_point_id"]; ok && v.(string) != "" {
				buf.WriteString(fmt.Sprintf("%s-", v.(string)))
			}
			if v, ok := m["iam"]; ok && v.(string) != "" {
				buf.WriteString(fmt.Sprintf("%s-", v.(string)))
			}
		}
	}

	if v, ok := m["fsx_windows_file_server_volume_configuration"]; ok && len(v.([]interface{})) > 0 && v.([]interface{})[0] != nil {
		m := v.([]interface{})[0].(map[string]interface{})

		if v, ok := m["file_system_id"]; ok && v.(string) != "" {
			buf.WriteString(fmt.Sprintf("%s-", v.(string)))
		}

		if v, ok := m["root_directory"]; ok && v.(string) != "" {
			buf.WriteString(fmt.Sprintf("%s-", v.(string)))
		}

		if v, ok := m["authorization_config"]; ok && len(v.([]interface{})) > 0 && v.([]interface{})[0] != nil {
			m := v.([]interface{})[0].(map[string]interface{})
			if v, ok := m["credentials_parameter"]; ok && v.(string) != "" {
				buf.WriteString(fmt.Sprintf("%s-", v.(string)))
			}
			if v, ok := m["domain"]; ok && v.(string) != "" {
				buf.WriteString(fmt.Sprintf("%s-", v.(string)))
			}
		}
	}

	return create.StringHashcode(buf.String())
}

func flattenPlacementConstraints(pcs []*ecs.TaskDefinitionPlacementConstraint) []map[string]interface{} {
	if len(pcs) == 0 {
		return nil
	}
	results := make([]map[string]interface{}, 0)
	for _, pc := range pcs {
		c := make(map[string]interface{})
		c["type"] = aws.StringValue(pc.Type)
		c["expression"] = aws.StringValue(pc.Expression)
		results = append(results, c)
	}
	return results
}

func flattenRuntimePlatform(rp *ecs.RuntimePlatform) []map[string]interface{} {
	if rp == nil {
		return nil
	}

	os := aws.StringValue(rp.OperatingSystemFamily)
	cpu := aws.StringValue(rp.CpuArchitecture)

	if os == "" && cpu == "" {
		return nil
	}

	config := make(map[string]interface{})

	if os != "" {
		config["operating_system_family"] = os
	}
	if cpu != "" {
		config["cpu_architecture"] = cpu
	}

	return []map[string]interface{}{
		config,
	}
}

func flattenProxyConfiguration(pc *ecs.ProxyConfiguration) []map[string]interface{} {
	if pc == nil {
		return nil
	}

	meshProperties := make(map[string]string)
	if pc.Properties != nil {
		for _, prop := range pc.Properties {
			meshProperties[aws.StringValue(prop.Name)] = aws.StringValue(prop.Value)
		}
	}

	config := make(map[string]interface{})
	config["container_name"] = aws.StringValue(pc.ContainerName)
	config["type"] = aws.StringValue(pc.Type)
	config["properties"] = meshProperties

	return []map[string]interface{}{
		config,
	}
}

func flattenInferenceAccelerators(list []*ecs.InferenceAccelerator) []map[string]interface{} {
	result := make([]map[string]interface{}, 0, len(list))
	for _, iAcc := range list {
		l := map[string]interface{}{
			"device_name": aws.StringValue(iAcc.DeviceName),
			"device_type": aws.StringValue(iAcc.DeviceType),
		}

		result = append(result, l)
	}
	return result
}

func expandInferenceAccelerators(configured []interface{}) []*ecs.InferenceAccelerator {
	iAccs := make([]*ecs.InferenceAccelerator, 0, len(configured))
	for _, lRaw := range configured {
		data := lRaw.(map[string]interface{})
		l := &ecs.InferenceAccelerator{
			DeviceName: aws.String(data["device_name"].(string)),
			DeviceType: aws.String(data["device_type"].(string)),
		}
		iAccs = append(iAccs, l)
	}

	return iAccs
}

func expandTaskDefinitionPlacementConstraints(constraints []interface{}) ([]*ecs.TaskDefinitionPlacementConstraint, error) {
	var pc []*ecs.TaskDefinitionPlacementConstraint
	for _, raw := range constraints {
		p := raw.(map[string]interface{})
		t := p["type"].(string)
		e := p["expression"].(string)
		if err := validPlacementConstraint(t, e); err != nil {
			return nil, err
		}
		pc = append(pc, &ecs.TaskDefinitionPlacementConstraint{
			Type:       aws.String(t),
			Expression: aws.String(e),
		})
	}

	return pc, nil
}

func expandTaskDefinitionRuntimePlatformConfiguration(runtimePlatformConfig []interface{}) *ecs.RuntimePlatform {
	config := runtimePlatformConfig[0]

	configMap := config.(map[string]interface{})
	ecsProxyConfig := &ecs.RuntimePlatform{}

	os := configMap["operating_system_family"].(string)
	if os != "" {
		ecsProxyConfig.OperatingSystemFamily = aws.String(os)
	}

	osFamily := configMap["cpu_architecture"].(string)
	if osFamily != "" {
		ecsProxyConfig.CpuArchitecture = aws.String(osFamily)
	}

	return ecsProxyConfig
}

func expandTaskDefinitionProxyConfiguration(proxyConfigs []interface{}) *ecs.ProxyConfiguration {
	proxyConfig := proxyConfigs[0]
	configMap := proxyConfig.(map[string]interface{})

	rawProperties := configMap["properties"].(map[string]interface{})

	properties := make([]*ecs.KeyValuePair, len(rawProperties))
	i := 0
	for name, value := range rawProperties {
		properties[i] = &ecs.KeyValuePair{
			Name:  aws.String(name),
			Value: aws.String(value.(string)),
		}
		i++
	}

	ecsProxyConfig := &ecs.ProxyConfiguration{
		ContainerName: aws.String(configMap["container_name"].(string)),
		Type:          aws.String(configMap["type"].(string)),
		Properties:    properties,
	}

	return ecsProxyConfig
}

func expandVolumes(configured []interface{}) []*ecs.Volume {
	volumes := make([]*ecs.Volume, 0, len(configured))

	// Loop over our configured volumes and create
	// an array of aws-sdk-go compatible objects
	for _, lRaw := range configured {
		data := lRaw.(map[string]interface{})

		l := &ecs.Volume{
			Name: aws.String(data["name"].(string)),
		}

		hostPath := data["host_path"].(string)
		if hostPath != "" {
			l.Host = &ecs.HostVolumeProperties{
				SourcePath: aws.String(hostPath),
			}
		}

		if v, ok := data["docker_volume_configuration"].([]interface{}); ok && len(v) > 0 {
			l.DockerVolumeConfiguration = expandVolumesDockerVolume(v)
		}

		if v, ok := data["efs_volume_configuration"].([]interface{}); ok && len(v) > 0 {
			l.EfsVolumeConfiguration = expandVolumesEFSVolume(v)
		}

		if v, ok := data["fsx_windows_file_server_volume_configuration"].([]interface{}); ok && len(v) > 0 {
			l.FsxWindowsFileServerVolumeConfiguration = expandVolumesFSxWinVolume(v)
		}

		volumes = append(volumes, l)
	}

	return volumes
}

func expandVolumesDockerVolume(configList []interface{}) *ecs.DockerVolumeConfiguration {
	config := configList[0].(map[string]interface{})
	dockerVol := &ecs.DockerVolumeConfiguration{}

	if v, ok := config["scope"].(string); ok && v != "" {
		dockerVol.Scope = aws.String(v)
	}

	if v, ok := config["autoprovision"]; ok && v != "" {
		if dockerVol.Scope == nil || aws.StringValue(dockerVol.Scope) != ecs.ScopeTask || v.(bool) {
			dockerVol.Autoprovision = aws.Bool(v.(bool))
		}
	}

	if v, ok := config["driver"].(string); ok && v != "" {
		dockerVol.Driver = aws.String(v)
	}

	if v, ok := config["driver_opts"].(map[string]interface{}); ok && len(v) > 0 {
		dockerVol.DriverOpts = flex.ExpandStringMap(v)
	}

	if v, ok := config["labels"].(map[string]interface{}); ok && len(v) > 0 {
		dockerVol.Labels = flex.ExpandStringMap(v)
	}

	return dockerVol
}

func expandVolumesEFSVolume(efsConfig []interface{}) *ecs.EFSVolumeConfiguration {
	config := efsConfig[0].(map[string]interface{})
	efsVol := &ecs.EFSVolumeConfiguration{}

	if v, ok := config["file_system_id"].(string); ok && v != "" {
		efsVol.FileSystemId = aws.String(v)
	}

	if v, ok := config["root_directory"].(string); ok && v != "" {
		efsVol.RootDirectory = aws.String(v)
	}
	if v, ok := config["transit_encryption"].(string); ok && v != "" {
		efsVol.TransitEncryption = aws.String(v)
	}

	if v, ok := config["transit_encryption_port"].(int); ok && v > 0 {
		efsVol.TransitEncryptionPort = aws.Int64(int64(v))
	}
	if v, ok := config["authorization_config"].([]interface{}); ok && len(v) > 0 {
		efsVol.AuthorizationConfig = expandVolumesEFSVolumeAuthorizationConfig(v)
	}

	return efsVol
}

func expandVolumesEFSVolumeAuthorizationConfig(efsConfig []interface{}) *ecs.EFSAuthorizationConfig {
	authconfig := efsConfig[0].(map[string]interface{})
	auth := &ecs.EFSAuthorizationConfig{}

	if v, ok := authconfig["access_point_id"].(string); ok && v != "" {
		auth.AccessPointId = aws.String(v)
	}

	if v, ok := authconfig["iam"].(string); ok && v != "" {
		auth.Iam = aws.String(v)
	}

	return auth
}

func expandVolumesFSxWinVolume(fsxWinConfig []interface{}) *ecs.FSxWindowsFileServerVolumeConfiguration {
	config := fsxWinConfig[0].(map[string]interface{})
	fsxVol := &ecs.FSxWindowsFileServerVolumeConfiguration{}

	if v, ok := config["file_system_id"].(string); ok && v != "" {
		fsxVol.FileSystemId = aws.String(v)
	}

	if v, ok := config["root_directory"].(string); ok && v != "" {
		fsxVol.RootDirectory = aws.String(v)
	}

	if v, ok := config["authorization_config"].([]interface{}); ok && len(v) > 0 {
		fsxVol.AuthorizationConfig = expandVolumesFSxWinVolumeAuthorizationConfig(v)
	}

	return fsxVol
}

func expandVolumesFSxWinVolumeAuthorizationConfig(config []interface{}) *ecs.FSxWindowsFileServerAuthorizationConfig {
	authconfig := config[0].(map[string]interface{})
	auth := &ecs.FSxWindowsFileServerAuthorizationConfig{}

	if v, ok := authconfig["credentials_parameter"].(string); ok && v != "" {
		auth.CredentialsParameter = aws.String(v)
	}

	if v, ok := authconfig["domain"].(string); ok && v != "" {
		auth.Domain = aws.String(v)
	}

	return auth
}

func flattenVolumes(list []*ecs.Volume) []map[string]interface{} {
	result := make([]map[string]interface{}, 0, len(list))
	for _, volume := range list {
		l := map[string]interface{}{
			"name": aws.StringValue(volume.Name),
		}

		if volume.Host != nil && volume.Host.SourcePath != nil {
			l["host_path"] = aws.StringValue(volume.Host.SourcePath)
		}

		if volume.DockerVolumeConfiguration != nil {
			l["docker_volume_configuration"] = flattenDockerVolumeConfiguration(volume.DockerVolumeConfiguration)
		}

		if volume.EfsVolumeConfiguration != nil {
			l["efs_volume_configuration"] = flattenEFSVolumeConfiguration(volume.EfsVolumeConfiguration)
		}

		if volume.FsxWindowsFileServerVolumeConfiguration != nil {
			l["fsx_windows_file_server_volume_configuration"] = flattenFSxWinVolumeConfiguration(volume.FsxWindowsFileServerVolumeConfiguration)
		}

		result = append(result, l)
	}
	return result
}

func flattenDockerVolumeConfiguration(config *ecs.DockerVolumeConfiguration) []interface{} {
	var items []interface{}
	m := make(map[string]interface{})

	if v := config.Scope; v != nil {
		m["scope"] = aws.StringValue(v)
	}

	if v := config.Autoprovision; v != nil {
		m["autoprovision"] = aws.BoolValue(v)
	}

	if v := config.Driver; v != nil {
		m["driver"] = aws.StringValue(v)
	}

	if config.DriverOpts != nil {
		m["driver_opts"] = flex.FlattenStringMap(config.DriverOpts)
	}

	if v := config.Labels; v != nil {
		m["labels"] = flex.FlattenStringMap(v)
	}

	items = append(items, m)
	return items
}

func flattenEFSVolumeConfiguration(config *ecs.EFSVolumeConfiguration) []interface{} {
	var items []interface{}
	m := make(map[string]interface{})
	if config != nil {
		if v := config.FileSystemId; v != nil {
			m["file_system_id"] = aws.StringValue(v)
		}

		if v := config.RootDirectory; v != nil {
			m["root_directory"] = aws.StringValue(v)
		}
		if v := config.TransitEncryption; v != nil {
			m["transit_encryption"] = aws.StringValue(v)
		}

		if v := config.TransitEncryptionPort; v != nil {
			m["transit_encryption_port"] = int(aws.Int64Value(v))
		}

		if v := config.AuthorizationConfig; v != nil {
			m["authorization_config"] = flattenEFSVolumeAuthorizationConfig(v)
		}
	}

	items = append(items, m)
	return items
}

func flattenEFSVolumeAuthorizationConfig(config *ecs.EFSAuthorizationConfig) []interface{} {
	var items []interface{}
	m := make(map[string]interface{})
	if config != nil {
		if v := config.AccessPointId; v != nil {
			m["access_point_id"] = aws.StringValue(v)
		}
		if v := config.Iam; v != nil {
			m["iam"] = aws.StringValue(v)
		}
	}

	items = append(items, m)
	return items
}

func flattenFSxWinVolumeConfiguration(config *ecs.FSxWindowsFileServerVolumeConfiguration) []interface{} {
	var items []interface{}
	m := make(map[string]interface{})
	if config != nil {
		if v := config.FileSystemId; v != nil {
			m["file_system_id"] = aws.StringValue(v)
		}

		if v := config.RootDirectory; v != nil {
			m["root_directory"] = aws.StringValue(v)
		}

		if v := config.AuthorizationConfig; v != nil {
			m["authorization_config"] = flattenFSxWinVolumeAuthorizationConfig(v)
		}
	}

	items = append(items, m)
	return items
}

func flattenFSxWinVolumeAuthorizationConfig(config *ecs.FSxWindowsFileServerAuthorizationConfig) []interface{} {
	var items []interface{}
	m := make(map[string]interface{})
	if config != nil {
		if v := config.CredentialsParameter; v != nil {
			m["credentials_parameter"] = aws.StringValue(v)
		}
		if v := config.Domain; v != nil {
			m["domain"] = aws.StringValue(v)
		}
	}

	items = append(items, m)
	return items
}

func flattenContainerDefinitions(definitions []*ecs.ContainerDefinition) (string, error) {
	b, err := jsonutil.BuildJSON(definitions)
	if err != nil {
		return "", err
	}

	return string(b), nil
}

func expandContainerDefinitions(rawDefinitions string) ([]*ecs.ContainerDefinition, error) {
	var definitions []*ecs.ContainerDefinition

	err := json.Unmarshal([]byte(rawDefinitions), &definitions)
	if err != nil {
		return nil, fmt.Errorf("decoding JSON: %s", err)
	}

	for i, c := range definitions {
		if c == nil {
			return nil, fmt.Errorf("invalid container definition supplied at index (%d)", i)
		}
	}

	return definitions, nil
}

func expandEnvironment(l []interface{}) []*ecs.KeyValuePair {
	var environment []*ecs.KeyValuePair

	for _, raw := range l {
		data := raw.(map[string]interface{})
		environment = append(environment, &ecs.KeyValuePair{
			Name:  aws.String(data["name"].(string)),
			Value: aws.String(data["value"].(string)),
		})
	}

	return environment
}

func expandPortMappings(l []interface{}) []*ecs.PortMapping {
	var portMappings []*ecs.PortMapping

	for _, raw := range l {
		if raw == nil {
			continue
		}

		data := raw.(map[string]interface{})
		portMapping := &ecs.PortMapping{
			ContainerPort: aws.Int64(int64(data["container_port"].(int))),
		}

		if v, ok := data["host_port"].(int); ok {
			portMapping.HostPort = aws.Int64(int64(v))
		}
		if v, ok := data["protocol"].(string); ok {
			portMapping.Protocol = aws.String(v)
		}

		portMappings = append(portMappings, portMapping)
	}

	return portMappings
}

func expandContainerDefinitionsStructured(l []interface{}) []*ecs.ContainerDefinition {
	var definitions []*ecs.ContainerDefinition

	for _, raw := range l {
		if raw == nil {
			continue
		}

		data := raw.(map[string]interface{})
		definition := &ecs.ContainerDefinition{
			Name:  aws.String(data["name"].(string)),
			Image: aws.String(data["image"].(string)),
		}

		// Optional fields should be checked for existence before assignment
		if v, ok := data["cpu"].(int); ok {
			definition.Cpu = aws.Int64(int64(v))
		}
		if v, ok := data["command"].([]interface{}); ok {
			definition.Command = flex.ExpandStringList(v)
		}
		if v, ok := data["entry_point"].([]interface{}); ok {
			definition.EntryPoint = flex.ExpandStringList(v)
		}
		if v, ok := data["essential"].(bool); ok {
			definition.Essential = aws.Bool(v)
		}
		if v, ok := data["image"].(string); ok {
			definition.Image = aws.String(v)
		}
		if v, ok := data["links"].([]interface{}); ok {
			definition.Links = flex.ExpandStringList(v)
		}
		if v, ok := data["memory"].(int); ok {
			definition.Memory = aws.Int64(int64(v))
		}
		if v, ok := data["name"].(string); ok {
			definition.Name = aws.String(v)
		}
		if v, ok := data["environment"].([]interface{}); ok {
			definition.Environment = expandEnvironment(v)
		}
		if v, ok := data["port_mapping"].([]interface{}); ok {
			definition.PortMappings = expandPortMappings(v)
		}

		definitions = append(definitions, definition)
	}

	return definitions
}

func expandTaskDefinitionEphemeralStorage(config []interface{}) *ecs.EphemeralStorage {
	configMap := config[0].(map[string]interface{})

	es := &ecs.EphemeralStorage{
		SizeInGiB: aws.Int64(int64(configMap["size_in_gib"].(int))),
	}

	return es
}

func flattenTaskDefinitionEphemeralStorage(pc *ecs.EphemeralStorage) []map[string]interface{} {
	if pc == nil {
		return nil
	}

	m := make(map[string]interface{})
	m["size_in_gib"] = aws.Int64Value(pc.SizeInGiB)

	return []map[string]interface{}{m}
}

// StripRevision strips the trailing revision number from a task definition ARN
//
// Invalid ARNs will return an empty string. ARNs with an unexpected number of
// separators in the resource section are returned unmodified.
func StripRevision(s string) string {
	tdArn, err := arn.Parse(s)
	if err != nil {
		return ""
	}
	parts := strings.Split(tdArn.Resource, ":")
	if len(parts) == 2 {
		tdArn.Resource = parts[0]
	}
	return tdArn.String()
}
