// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package ecs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
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
				Optional: true,

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
			"container_definition": {
				Type:     schema.TypeList,
				Required: true,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"command": {
							Type:        schema.TypeList,
							Optional:    true,
							Description: "The command that's passed to the container.",
							Elem: &schema.Schema{
								Type: schema.TypeString,
							},
						},
						"cpu": {
							Type:        schema.TypeInt,
							Optional:    true,
							Description: "The number of cpu units reserved for the container.",
						},
						"credential_specs": {
							Type:        schema.TypeList,
							Optional:    true,
							Description: "A list of ARNs in SSM or Amazon S3 to a credential spec (CredSpec) file that configures the container for Active Directory authentication.",
							Elem: &schema.Schema{
								Type: schema.TypeString,
							},
						},
						"depends_on": {
							Type:        schema.TypeList,
							Optional:    true,
							Description: "The dependencies defined for container startup and shutdown.",
							Elem: &schema.Resource{
								Schema: map[string]*schema.Schema{
									"condition": {
										Type:        schema.TypeString,
										Required:    true,
										Description: "The dependency condition of the container.",
									},
									"container_name": {
										Type:        schema.TypeString,
										Required:    true,
										Description: "The name of a container.",
									},
								},
							},
						},
						"disable_networking": {
							Type:        schema.TypeBool,
							Optional:    true,
							Description: "When this parameter is true, networking is off within the container.",
						},
						"dns_search_domains": {
							Type:        schema.TypeList,
							Optional:    true,
							Description: "A list of DNS search domains that are presented to the container.",
							Elem: &schema.Schema{
								Type: schema.TypeString,
							},
						},
						"dns_servers": {
							Type:        schema.TypeList,
							Optional:    true,
							Description: "A list of DNS servers that are presented to the container.",
							Elem: &schema.Schema{
								Type: schema.TypeString,
							},
						},
						"docker_labels": {
							Type:     schema.TypeMap,
							Elem:     &schema.Schema{Type: schema.TypeString},
							ForceNew: true,
							Optional: true,
						},
						"docker_security_options": {
							Type:        schema.TypeList,
							Optional:    true,
							Description: "A list of strings to provide custom configuration for multiple security systems.",
							Elem: &schema.Schema{
								Type: schema.TypeString,
							},
						},
						"entry_point": {
							Type:        schema.TypeList,
							Optional:    true,
							Description: "The entry point that's passed to the container.",
							Elem: &schema.Schema{
								Type: schema.TypeString,
							},
						},
						"environment": {
							Type:        schema.TypeList,
							Optional:    true,
							Description: "The environment variables to pass to a container.",
							Elem: &schema.Resource{
								Schema: map[string]*schema.Schema{
									"name": {
										Type:     schema.TypeString,
										Required: true,
									},
									"value": {
										Type:     schema.TypeString,
										Optional: true,
									},
								},
							},
						},
						"environment_files": {
							Type:        schema.TypeList,
							Optional:    true,
							Description: "A list of files containing the environment variables to pass to a container.",
							Elem: &schema.Resource{
								Schema: map[string]*schema.Schema{
									"type": {
										Type:     schema.TypeString,
										Required: true,
									},
									"value": {
										Type:     schema.TypeString,
										Optional: true,
									},
								},
							},
						},
						"essential": {
							Type:        schema.TypeBool,
							Optional:    true,
							Description: "If the essential parameter of a container is marked as true, and that container fails or stops for any reason, all other containers that are part of the task are stopped. If the essential parameter of a container is marked as false, its failure doesn't affect the rest of the containers in a task. If this parameter is omitted, a container is assumed to be essential.",
						},
						"extra_hosts": {
							Type:        schema.TypeList,
							Optional:    true,
							Description: "A list of hostnames that are attached to the container.",
							Elem: &schema.Resource{
								Schema: map[string]*schema.Schema{
									"hostname": {
										Type:     schema.TypeString,
										Required: true,
									},
									"ip_address": {
										Type:     schema.TypeString,
										Required: true,
									},
								},
							},
						},
						"firelens_configuration": {
							Type:        schema.TypeList,
							MaxItems:    1,
							Optional:    true,
							Description: "The FireLens configuration for the container.",
							Elem: &schema.Resource{
								Schema: map[string]*schema.Schema{
									"options": {
										Type: schema.TypeMap,
										Elem: &schema.Schema{
											Type: schema.TypeString,
										},
										Optional: true,
									},
									"type": {
										Type:     schema.TypeString,
										Required: true,
									},
								},
							},
						},
						"health_check": {
							Type:        schema.TypeList,
							MaxItems:    1,
							Optional:    true,
							Description: "The container health check command and associated configuration parameters for the container.",
							Elem: &schema.Resource{
								Schema: map[string]*schema.Schema{
									"command": {
										Type: schema.TypeList,
										Elem: &schema.Schema{
											Type: schema.TypeString,
										},
										Required: true,
									},
									"interval": {
										Type:     schema.TypeInt,
										Optional: true,
									},
									"retries": {
										Type:     schema.TypeInt,
										Optional: true,
									},
									"start_period": {
										Type:     schema.TypeInt,
										Optional: true,
									},
									"timeout": {
										Type:     schema.TypeInt,
										Optional: true,
									},
								},
							},
						},
						"hostname": {
							Type:        schema.TypeString,
							Optional:    true,
							Description: "The hostname to use for your container.",
						},
						"image": {
							Type:        schema.TypeString,
							Optional:    true,
							Description: "The image used to start a container.",
						},
						"interactive": {
							Type:        schema.TypeBool,
							Optional:    true,
							Description: "When this parameter is true, you can deploy containerized applications that require stdin or a tty to be allocated.",
						},
						"links": {
							Type:        schema.TypeList,
							Optional:    true,
							Description: "The links parameter allows containers to communicate with each other without the need for port mappings.",
							Elem: &schema.Schema{
								Type: schema.TypeString,
							},
						},
						"linux_parameters": {
							Type:        schema.TypeList,
							MaxItems:    1,
							Optional:    true,
							Description: "A list of Linux parameters to pass to the container.",
							Elem: &schema.Resource{
								Schema: map[string]*schema.Schema{
									"capabilities": {
										Type:        schema.TypeList,
										MaxItems:    1,
										Optional:    true,
										Description: "The Linux capabilities for the container that are added to or dropped from the default configuration provided by Docker.",
										Elem: &schema.Resource{
											Schema: map[string]*schema.Schema{
												"add": {
													Type:     schema.TypeList,
													Optional: true,
													Elem: &schema.Schema{
														Type: schema.TypeString,
													},
												},
												"drop": {
													Type:     schema.TypeList,
													Optional: true,
													Elem: &schema.Schema{
														Type: schema.TypeString,
													},
												},
											},
										},
									},
									"devices": {
										Type:        schema.TypeList,
										MaxItems:    1,
										Optional:    true,
										Description: "Any host devices to expose to the container.",
										Elem: &schema.Resource{
											Schema: map[string]*schema.Schema{
												"container_path": {
													Type:     schema.TypeString,
													Optional: true,
												},
												"host_path": {
													Type:     schema.TypeString,
													Required: true,
												},
												"permissions": {
													Type:     schema.TypeList,
													Optional: true,
													Elem: &schema.Schema{
														Type: schema.TypeString,
													},
												},
											},
										},
									},
									"init_process_enabled": {
										Type:        schema.TypeBool,
										Optional:    true,
										Description: "Run an init process inside the container that forwards signals and reaps processes.",
									},
									"max_swap": {
										Type:        schema.TypeInt,
										Optional:    true,
										Description: "The total amount of swap memory (in MiB) a container can use.",
									},
									"shared_memory_size": {
										Type:        schema.TypeInt,
										Optional:    true,
										Description: "The value for the size (in MiB) of the /dev/shm volume.",
									},
									"swappiness": {
										Type:        schema.TypeInt,
										Optional:    true,
										Description: "This allows you to tune a container's memory swappiness behavior.",
									},
									"tmpfs": {
										Type:        schema.TypeList,
										Optional:    true,
										Description: "The container path, mount options, and size (in MiB) of the tmpfs mount.",
										Elem: &schema.Resource{
											Schema: map[string]*schema.Schema{
												"container_path": {
													Type:        schema.TypeString,
													Required:    true,
													Description: "The absolute file path where the tmpfs volume is to be mounted.",
												},
												"size": {
													Type:        schema.TypeInt,
													Required:    true,
													Description: "The maximum size (in MiB) of the tmpfs volume.",
												},
												"mount_options": {
													Type:        schema.TypeList,
													Optional:    true,
													Description: "The list of tmpfs volume mount options.",
													Elem: &schema.Schema{
														Type: schema.TypeString,
													},
												},
											},
										},
									},
								},
							},
						},
						"log_configuration": {
							Type:        schema.TypeList,
							MaxItems:    1,
							Optional:    true,
							Description: "The log configuration for the container.",
							Elem: &schema.Resource{
								Schema: map[string]*schema.Schema{
									"log_driver": {
										Type:        schema.TypeString,
										Required:    true,
										Description: "The log driver to use for the container.",
									},
									"options": {
										Type: schema.TypeMap,
										Elem: &schema.Schema{
											Type: schema.TypeString,
										},
										Optional:    true,
										Description: "The configuration options to send to the log driver.",
									},
									"secret_options": {
										Type:     schema.TypeList,
										MaxItems: 1,
										Optional: true,
										Elem: &schema.Schema{
											Type:     schema.TypeList,
											MaxItems: 1,
											Elem: &schema.Resource{
												Schema: map[string]*schema.Schema{
													"name": {
														Type:        schema.TypeString,
														Required:    true,
														Description: "The name of the secret.",
													},
													"value_from": {
														Type:        schema.TypeString,
														Required:    true,
														Description: "The secret to expose to the container.",
													},
												},
											},
										},
									},
								},
							},
						},
						"memory": {
							Type:        schema.TypeInt,
							Optional:    true,
							Description: "The amount (in MiB) of memory to present to the container.",
						},
						"memory_reservation": {
							Type:        schema.TypeInt,
							Optional:    true,
							Description: "The soft limit (in MiB) of memory to reserve for the container.",
						},
						"mount_points": {
							Type:        schema.TypeList,
							Optional:    true,
							Description: "The mount points for data volumes in your container.",
							Elem: &schema.Resource{
								Schema: map[string]*schema.Schema{
									"container_path": {
										Type:        schema.TypeString,
										Optional:    true,
										Description: "The path on the container to mount the host volume at.",
									},
									"read_only": {
										Type:        schema.TypeBool,
										Optional:    true,
										Description: "If this value is true, the container has read-only access to the volume.",
									},
									"source_volume": {
										Type:        schema.TypeString,
										Optional:    true,
										Description: "The name of the volume to mount.",
									},
								},
							},
						},
						"name": {
							Type:        schema.TypeString,
							Optional:    true,
							Description: "The name of a container.",
						},
						"port_mappings": {
							Type:        schema.TypeList,
							Optional:    true,
							Description: "The list of port mappings for the container.",
							Elem: &schema.Resource{
								Schema: map[string]*schema.Schema{
									"app_protocol": {
										Type:        schema.TypeString,
										Optional:    true,
										Description: "The application protocol that's used for the port mapping.",
									},
									"container_port": {
										Type:        schema.TypeInt,
										Optional:    true,
										Description: "The port number on the container that's bound to the user-specified or automatically assigned host port.",
									},
									"container_port_range": {
										Type:        schema.TypeString,
										Optional:    true,
										Description: "The port number range on the container that's bound to the dynamically mapped host port range.",
									},
									"host_port": {
										Type:        schema.TypeInt,
										Optional:    true,
										Description: "The port number on the container instance to reserve for your container.",
									},
									"name": {
										Type:        schema.TypeString,
										Optional:    true,
										Description: "The name that's used for the port mapping.",
									},
									"protocol": {
										Type:        schema.TypeString,
										Optional:    true,
										Description: "The protocol used for the port mapping.",
									},
								},
							},
						},
						"privileged": {
							Type:        schema.TypeBool,
							Optional:    true,
							Description: "When this parameter is true, the container is given elevated privileges on the host container instance (similar to the root user).",
						},
						"pseudo_terminal": {
							Type:        schema.TypeBool,
							Optional:    true,
							Description: "When this parameter is true, a TTY is allocated.",
						},
						"readonly_root_filesystem": {
							Type:        schema.TypeBool,
							Optional:    true,
							Description: "When this parameter is true, the container is given read-only access to its root file system.",
						},
						"repository_credentials": {
							Type:        schema.TypeList,
							MaxItems:    1,
							Optional:    true,
							Description: "The private repository authentication credentials to use.",
							Elem: &schema.Resource{
								Schema: map[string]*schema.Schema{
									"credentials_parameter": {
										Type:        schema.TypeString,
										Required:    true,
										Description: "The Amazon Resource Name (ARN) of the secret containing the private repository credentials.",
									},
								},
							},
						},
						"resource_requirements": {
							Type:        schema.TypeList,
							Optional:    true,
							Description: "The type and amount of a resource to assign to a container.",
							Elem: &schema.Resource{
								Schema: map[string]*schema.Schema{
									"type": {
										Type:        schema.TypeString,
										Required:    true,
										Description: "The type of resource to assign to a container.",
									},
									"value": {
										Type:        schema.TypeString,
										Required:    true,
										Description: "The value for the specified resource type.",
									},
								},
							},
						},
						"secrets": {
							Type:        schema.TypeList,
							Optional:    true,
							Description: "The secrets to pass to the container.",
							Elem: &schema.Resource{
								Schema: map[string]*schema.Schema{
									"name": {
										Type:     schema.TypeString,
										Required: true,
									},
									"value_from": {
										Type:     schema.TypeString,
										Required: true,
									},
								},
							},
						},
						"start_timeout": {
							Type:        schema.TypeInt,
							Optional:    true,
							Description: "Time duration (in seconds) to wait before giving up on resolving dependencies for a container.",
						},
						"stop_timeout": {
							Type:        schema.TypeInt,
							Optional:    true,
							Description: "Time duration (in seconds) to wait before the container is forcefully killed if it doesn't exit normally on its own.",
						},
						"system_controls": {
							Type:        schema.TypeList,
							Optional:    true,
							Description: "A list of namespaced kernel parameters to set in the container.",
							Elem: &schema.Resource{
								Schema: map[string]*schema.Schema{
									"namespace": {
										Type:     schema.TypeString,
										Optional: true,
									},
									"value": {
										Type:     schema.TypeString,
										Optional: true,
									},
								},
							},
						},
						"ulimits": {
							Type:        schema.TypeList,
							Optional:    true,
							Description: "A list of ulimits to set in the container.",
							Elem: &schema.Resource{
								Schema: map[string]*schema.Schema{
									"hard_limit": {
										Type:     schema.TypeInt,
										Required: true,
									},
									"name": {
										Type:     schema.TypeString,
										Required: true,
									},
									"soft_limit": {
										Type:     schema.TypeInt,
										Required: true,
									},
								},
							},
						},
						"user": {
							Type:        schema.TypeString,
							Optional:    true,
							Description: "The user to use inside the container.",
						},
						"volume_from": {
							Type:        schema.TypeList,
							Optional:    true,
							Description: "Data volumes to mount from another container.",
							Elem: &schema.Resource{
								Schema: map[string]*schema.Schema{
									"read_only": {
										Type:     schema.TypeBool,
										Optional: true,
									},
									"source_container": {
										Type:     schema.TypeString,
										Optional: true,
									},
								},
							},
						},
						"wodking_directory": {
							Type:        schema.TypeString,
							Optional:    true,
							Description: "The working directory to run commands inside the container in.",
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

	//rawDefinitions := d.Get("container_definitions").(string)
	//containerDefinitionsJSON, err := expandContainerDefinitions(rawDefinitions)
	//if err != nil {
	//	// TODO improve error message
	//	return sdkdiag.AppendErrorf(diags, "creating ECS Task Definition (%s): %s", d.Get("family").(string), err)
	//}

	// TODO: add logic for the precedence of formats

	v := d.Get("container_definition").([]interface{})
	containerDefinitionsStructured := expandContainerDefinitionsStructured(v)

	var containerDefinitions []*ecs.ContainerDefinition
	if containerDefinitionsStructured != nil {
		containerDefinitions = containerDefinitionsStructured
	} else {
		//containerDefinitions = containerDefinitionsJSON
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
	// TODO uncomment below
	//containerDefinitions(taskDefinition.ContainerDefinitions).OrderEnvironmentVariables()
	//containerDefinitions(taskDefinition.ContainerDefinitions).OrderSecrets()
	//
	//defs, err := flattenContainerDefinitions(taskDefinition.ContainerDefinitions)
	//if err != nil {
	//	return sdkdiag.AppendErrorf(diags, "reading ECS Task Definition (%s): %s", d.Id(), err)
	//}
	//err = d.Set("container_definitions", defs)
	//if err != nil {
	//	return sdkdiag.AppendErrorf(diags, "reading ECS Task Definition (%s): %s", d.Id(), err)
	//}

	defs, err := flattenContainerDefinitionsStructured(taskDefinition.ContainerDefinitions)
	if err != nil {
		return sdkdiag.AppendErrorf(diags, "reading ECS Task Definition (%s): %s", d.Id(), err)
	}
	err = d.Set("container_definition", defs)
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

func flattenEnvironment(env []*ecs.KeyValuePair) []map[string]interface{} {
	var l []map[string]interface{}

	for _, e := range env {
		entry := make(map[string]interface{})
		entry["name"] = aws.StringValue(e.Name)
		entry["value"] = aws.StringValue(e.Value)
		l = append(l, entry)
	}

	return l
}

func flattenPortMappings(portMappings []*ecs.PortMapping) []map[string]interface{} {
	var l []map[string]interface{}

	for _, p := range portMappings {
		portMapping := make(map[string]interface{})
		portMapping["container_port"] = aws.Int64Value(p.ContainerPort)

		if p.HostPort != nil {
			portMapping["host_port"] = aws.Int64Value(p.HostPort)
		}

		if p.Protocol != nil {
			portMapping["protocol"] = aws.StringValue(p.Protocol)
		}

		l = append(l, portMapping)
	}

	return l

}

func flattenContainerDefinitionsStructured(definitions []*ecs.ContainerDefinition) ([]interface{}, error) {
	var l []interface{}

	for _, d := range definitions {
		container := make(map[string]interface{})
		container["name"] = aws.StringValue(d.Name)
		container["image"] = aws.StringValue(d.Image)

		if d.Cpu != nil {
			// TODO polish conversion
			container["cpu"] = strconv.FormatInt(aws.Int64Value(d.Cpu), 10)
		}

		if d.Command != nil {
			container["command"] = d.Command
		}

		if d.EntryPoint != nil {
			container["entry_point"] = d.EntryPoint
		}

		if d.Essential != nil {
			container["essential"] = aws.BoolValue(d.Essential)
		}

		if d.Image != nil {
			container["image"] = aws.StringValue(d.Image)
		}

		if d.Links != nil {
			container["links"] = d.Links
		}

		if d.Memory != nil {
			container["memory"] = strconv.FormatInt(aws.Int64Value(d.Memory), 10)
		}

		if d.Environment != nil {
			container["environment"] = flattenEnvironment(d.Environment)
		}

		if d.PortMappings != nil {
			container["port_mappings"] = flattenPortMappings(d.PortMappings)
		}

		l = append(l, container)
	}

	return l, nil
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
			Name: aws.String(data["name"].(string)),
		}

		if v, ok := data["image"].(string); ok {
			definition.Image = aws.String(v)
		}
		if v, ok := data["cpu"].(int); ok {
			definition.Cpu = aws.Int64(int64(v))
		}
		if v, ok := data["memory"].(int); ok {
			definition.Memory = aws.Int64(int64(v))
		}
		if v, ok := data["essential"].(bool); ok {
			definition.Essential = aws.Bool(v)
		}
		if v, ok := data["environment"].([]interface{}); ok {
			definition.Environment = expandEnvironment(v)
		}
		if v, ok := data["port_mappings"].([]interface{}); ok {
			definition.PortMappings = expandPortMappings(v)
		}
		if v, ok := data["entry_point"].([]interface{}); ok {
			definition.EntryPoint = aws.StringSlice(expandStringList(v))
		}
		if v, ok := data["command"].([]interface{}); ok {
			definition.Command = aws.StringSlice(expandStringList(v))
		}
		if v, ok := data["volumes_from"].([]interface{}); ok {
			definition.VolumesFrom = expandVolumesFrom(v)
		}
		if v, ok := data["linux_parameters"].(map[string]interface{}); ok {
			definition.LinuxParameters = expandLinuxParameters(v)
		}
		if v, ok := data["secrets"].([]interface{}); ok {
			definition.Secrets = expandSecrets(v)
		}
		if v, ok := data["depends_on"].([]interface{}); ok {
			definition.DependsOn = expandContainerDependencies(v)
		}
		if v, ok := data["start_timeout"].(int); ok {
			definition.StartTimeout = aws.Int64(int64(v))
		}
		if v, ok := data["stop_timeout"].(int); ok {
			definition.StopTimeout = aws.Int64(int64(v))
		}
		if v, ok := data["hostname"].(string); ok {
			definition.Hostname = aws.String(v)
		}
		if v, ok := data["user"].(string); ok {
			definition.User = aws.String(v)
		}
		if v, ok := data["working_directory"].(string); ok {
			definition.WorkingDirectory = aws.String(v)
		}
		if v, ok := data["disable_networking"].(bool); ok {
			definition.DisableNetworking = aws.Bool(v)
		}
		if v, ok := data["privileged"].(bool); ok {
			definition.Privileged = aws.Bool(v)
		}
		if v, ok := data["readonly_root_filesystem"].(bool); ok {
			definition.ReadonlyRootFilesystem = aws.Bool(v)
		}
		if v, ok := data["dns_servers"].([]interface{}); ok {
			definition.DnsServers = aws.StringSlice(expandStringList(v))
		}
		if v, ok := data["dns_search_domains"].([]interface{}); ok {
			definition.DnsSearchDomains = aws.StringSlice(expandStringList(v))
		}
		if v, ok := data["extra_hosts"].([]interface{}); ok {
			definition.ExtraHosts = expandExtraHosts(v)
		}
		if v, ok := data["docker_security_options"].([]interface{}); ok {
			definition.DockerSecurityOptions = aws.StringSlice(expandStringList(v))
		}
		if v, ok := data["interactive"].(bool); ok {
			definition.Interactive = aws.Bool(v)
		}
		if v, ok := data["pseudo_terminal"].(bool); ok {
			definition.PseudoTerminal = aws.Bool(v)
		}
		if v, ok := data["docker_labels"].(map[string]interface{}); ok {
			stringMap := make(map[string]string)
			for key, value := range v {
				if strVal, ok := value.(string); ok {
					stringMap[key] = strVal
				}
			}
			definition.DockerLabels = aws.StringMap(stringMap)
		}
		if v, ok := data["ulimits"].([]interface{}); ok {
			definition.Ulimits = expandUlimits(v)
		}
		if v, ok := data["log_configuration"].(map[string]interface{}); ok {
			definition.LogConfiguration = expandLogConfigurationStructured(v)
		}
		if v, ok := data["health_check"].(map[string]interface{}); ok {
			definition.HealthCheck = expandHealthCheck(v)
		}
		if v, ok := data["system_controls"].([]interface{}); ok {
			definition.SystemControls = expandSystemControls(v)
		}
		if v, ok := data["resource_requirements"].([]interface{}); ok {
			definition.ResourceRequirements = expandResourceRequirementsStructured(v)
		}
		if v, ok := data["firelens_configuration"].(map[string]interface{}); ok {
			definition.FirelensConfiguration = expandFirelensConfiguration(v)
		}

		definitions = append(definitions, definition)
	}

	return definitions
}

// Implementing missing functions
func expandStringList(v []interface{}) []string {
	var list []string
	for _, item := range v {
		if str, ok := item.(string); ok {
			list = append(list, str)
		}
	}
	return list
}

func expandLogConfigurationStructured(config map[string]interface{}) *ecs.LogConfiguration {
	logConfig := &ecs.LogConfiguration{}

	if v, ok := config["log_driver"].(string); ok {
		logConfig.LogDriver = aws.String(v)
	}
	if v, ok := config["options"].(map[string]interface{}); ok {
		logConfig.Options = expandLogConfigurationOptions(v)
	}
	if v, ok := config["secret_options"].([]interface{}); ok {
		logConfig.SecretOptions = expandSecrets(v)
	}

	return logConfig
}

func expandLogConfigurationOptions(v map[string]interface{}) map[string]*string {
	options := make(map[string]*string)
	for key, value := range v {
		options[key] = aws.String(value.(string))
	}
	return options
}

func expandResourceRequirementsStructured(data []interface{}) []*ecs.ResourceRequirement {
	results := make([]*ecs.ResourceRequirement, 0, len(data))
	for _, raw := range data {
		item := raw.(map[string]interface{})
		req := &ecs.ResourceRequirement{
			Type:  aws.String(item["type"].(string)),  // 'type' is a required field
			Value: aws.String(item["value"].(string)), // 'value' is another field, assuming it's a string
		}
		results = append(results, req)
	}
	return results
}

func expandVolumesFrom(v []interface{}) []*ecs.VolumeFrom {
	var volumes []*ecs.VolumeFrom
	for _, item := range v {
		if mapItem, ok := item.(map[string]interface{}); ok {
			volume := &ecs.VolumeFrom{
				SourceContainer: aws.String(mapItem["source_container"].(string)),
				ReadOnly:        aws.Bool(mapItem["read_only"].(bool)),
			}
			volumes = append(volumes, volume)
		}
	}
	return volumes
}

func expandLinuxParameters(v map[string]interface{}) *ecs.LinuxParameters {
	return &ecs.LinuxParameters{
		// Assuming structure based on typical ECS LinuxParameters
		InitProcessEnabled: aws.Bool(v["init_process_enabled"].(bool)),
	}
}

func expandSecrets(v []interface{}) []*ecs.Secret {
	var secrets []*ecs.Secret
	for _, item := range v {
		if mapItem, ok := item.(map[string]interface{}); ok {
			secret := &ecs.Secret{
				Name:      aws.String(mapItem["name"].(string)),
				ValueFrom: aws.String(mapItem["value_from"].(string)),
			}
			secrets = append(secrets, secret)
		}
	}
	return secrets
}

func expandContainerDependencies(v []interface{}) []*ecs.ContainerDependency {
	var dependencies []*ecs.ContainerDependency
	for _, item := range v {
		if mapItem, ok := item.(map[string]interface{}); ok {
			dependency := &ecs.ContainerDependency{
				ContainerName: aws.String(mapItem["container_name"].(string)),
				Condition:     aws.String(mapItem["condition"].(string)),
			}
			dependencies = append(dependencies, dependency)
		}
	}
	return dependencies
}

func expandUlimits(v []interface{}) []*ecs.Ulimit {
	var ulimits []*ecs.Ulimit
	for _, item := range v {
		if mapItem, ok := item.(map[string]interface{}); ok {
			ulimit := &ecs.Ulimit{
				Name:      aws.String(mapItem["name"].(string)),
				SoftLimit: aws.Int64(int64(mapItem["soft_limit"].(int))),
				HardLimit: aws.Int64(int64(mapItem["hard_limit"].(int))),
			}
			ulimits = append(ulimits, ulimit)
		}
	}
	return ulimits
}

func expandHealthCheck(v map[string]interface{}) *ecs.HealthCheck {
	return &ecs.HealthCheck{
		Command:     aws.StringSlice(expandStringList(v["command"].([]interface{}))),
		Interval:    aws.Int64(int64(v["interval"].(int))),
		Timeout:     aws.Int64(int64(v["timeout"].(int))),
		Retries:     aws.Int64(int64(v["retries"].(int))),
		StartPeriod: aws.Int64(int64(v["start_period"].(int))),
	}
}

func expandSystemControls(v []interface{}) []*ecs.SystemControl {
	var controls []*ecs.SystemControl
	for _, item := range v {
		if mapItem, ok := item.(map[string]interface{}); ok {
			control := &ecs.SystemControl{
				Namespace: aws.String(mapItem["namespace"].(string)),
				Value:     aws.String(mapItem["value"].(string)),
			}
			controls = append(controls, control)
		}
	}
	return controls
}

func expandFirelensConfiguration(v map[string]interface{}) *ecs.FirelensConfiguration {
	return &ecs.FirelensConfiguration{
		Type:    aws.String(v["type"].(string)),
		Options: aws.StringMap(v["options"].(map[string]string)),
	}
}

func expandExtraHosts(v []interface{}) []*ecs.HostEntry {
	var hosts []*ecs.HostEntry
	for _, item := range v {
		if mapItem, ok := item.(map[string]interface{}); ok {
			host := &ecs.HostEntry{
				Hostname:  aws.String(mapItem["hostname"].(string)),
				IpAddress: aws.String(mapItem["ip_address"].(string)),
			}
			hosts = append(hosts, host)
		}
	}
	return hosts
}
func expandTaskDefinitionEphemeralStorage(config []interface{}) *ecs.EphemeralStorage {
	configMap := config[0].(map[string]interface{})

	es := &ecs.EphemeralStorage{
		SizeInGiB: aws.Int64(int64(configMap["size_in_gib"].(float64))),
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
