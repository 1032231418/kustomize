// Copyright 2019 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0

package filters

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"strings"

	"sigs.k8s.io/kustomize/kyaml/errors"
	"sigs.k8s.io/kustomize/kyaml/kio"
	"sigs.k8s.io/kustomize/kyaml/kio/kioutil"

	"sigs.k8s.io/kustomize/kyaml/yaml"
)

// ContainerFilter filters Resources using a container image.
// The container must start a process that reads the list of
// input Resources from stdin, reads the Configuration from the env
// API_CONFIG, and writes the filtered Resources to stdout.
// If there is a error or validation failure, the process must exit
// non-zero.
// The full set of environment variables from the parent process
// are passed to the container.
//
// Function Scoping:
// ContainerFilter applies the function only to Resources to which it is scoped.
//
// Resources are scoped to a function if any of the following are true:
// - the Resource were read from the same directory as the function config
// - the Resource were read from a subdirectory of the function config directory
// - the function config is in a directory named "functions" and
//   they were read from a subdirectory of "functions" parent
// - the function config doesn't have a path annotation (considered globally scoped)
// - the ContainerFilter has GlobalScope == true
//
// In Scope Examples:
//
// Example 1: deployment.yaml and service.yaml in function.yaml scope
//            same directory as the function config directory
//     .
//     ├── function.yaml
//     ├── deployment.yaml
//     └── service.yaml
//
// Example 2: apps/deployment.yaml and apps/service.yaml in function.yaml scope
//            subdirectory of the function config directory
//     .
//     ├── function.yaml
//     └── apps
//         ├── deployment.yaml
//         └── service.yaml
//
// Example 3: apps/deployment.yaml and apps/service.yaml in functions/function.yaml scope
//            function config is in a directory named "functions"
//     .
//     ├── functions
//     │   └── function.yaml
//     └── apps
//         ├── deployment.yaml
//         └── service.yaml
//
// Out of Scope Examples:
//
// Example 1: apps/deployment.yaml and apps/service.yaml NOT in stuff/function.yaml scope
//     .
//     ├── stuff
//     │   └── function.yaml
//     └── apps
//         ├── deployment.yaml
//         └── service.yaml
//
// Example 2: apps/deployment.yaml and apps/service.yaml NOT in stuff/functions/function.yaml scope
//     .
//     ├── stuff
//     │   └── functions
//     │       └── function.yaml
//     └── apps
//         ├── deployment.yaml
//         └── service.yaml
//
// Default Paths:
// Resources emitted by functions will have default path applied as annotations
// if none is present.
// The default path will be the function-dir/ (or parent directory in the case of "functions")
// + function-file-name/ + namespace/ + kind_name.yaml
//
// Example 1: Given a function in fn.yaml that produces a Deployment name foo and a Service named bar
//     dir
//     └── fn.yaml
//
// Would default newly generated Resources to:
//
//     dir
//     ├── fn.yaml
//     └── fn
//         ├── deployment_foo.yaml
//         └── service_bar.yaml
//
// Example 2: Given a function in functions/fn.yaml that produces a Deployment name foo and a Service named bar
//     dir
//     └── fn.yaml
//
// Would default newly generated Resources to:
//
//     dir
//     ├── functions
//     │   └── fn.yaml
//     └── fn
//         ├── deployment_foo.yaml
//         └── service_bar.yaml
//
// Example 3: Given a function in fn.yaml that produces a Deployment name foo, namespace baz and a Service named bar namespace baz
//     dir
//     └── fn.yaml
//
// Would default newly generated Resources to:
//
//     dir
//     ├── fn.yaml
//     └── fn
//         └── baz
//             ├── deployment_foo.yaml
//             └── service_bar.yaml
type ContainerFilter struct {

	// Image is the container image to use to create a container.
	Image string `yaml:"image,omitempty"`

	// Network is the container network to use.
	Network string `yaml:"network,omitempty"`

	// StorageMounts is a list of storage options that the container will have mounted.
	StorageMounts []StorageMount `yaml:"mounts,omitempty"`

	// Config is the API configuration for the container and passed through the
	// API_CONFIG env var to the container.
	// Typically a Kubernetes style Resource Config.
	Config *yaml.RNode `yaml:"config,omitempty"`

	// GlobalScope will cause the function to be run against all input
	// nodes instead of only nodes scoped under the function.
	GlobalScope bool

	ResultsFile string

	Results *yaml.RNode

	DeferFailure bool

	Exit error

	// SetFlowStyleForConfig sets the style for config to Flow when serializing it
	SetFlowStyleForConfig bool

	// args may be specified by tests to override how a container is spawned
	args []string

	checkInput func(string)
}

func (c ContainerFilter) GetExit() error {
	return c.Exit
}

type DeferFailureFunction interface {
	GetExit() error
}

func (c ContainerFilter) String() string {
	if c.DeferFailure {
		return fmt.Sprintf("%s deferFailure: %v", c.Image, c.DeferFailure)
	}
	return c.Image
}

func (s *StorageMount) String() string {
	return fmt.Sprintf("type=%s,src=%s,dst=%s:ro", s.MountType, s.Src, s.DstPath)
}

func StringToStorageMount(s string) StorageMount {
	m := make(map[string]string)
	options := strings.Split(s, ",")
	for _, option := range options {
		keyVal := strings.SplitN(option, "=", 2)
		m[keyVal[0]] = keyVal[1]
	}
	var sm StorageMount
	for key, value := range m {
		switch {
		case key == "type":
			sm.MountType = value
		case key == "src":
			sm.Src = value
		case key == "dst":
			sm.DstPath = value
		}
	}
	return sm
}

// functionsDirectoryName is keyword directory name for functions scoped 1 directory higher
const functionsDirectoryName = "functions"

// getFunctionScope returns the path of the directory containing the function config,
// or its parent directory if the base directory is named "functions"
func (c *ContainerFilter) getFunctionScope() (string, error) {
	m, err := c.Config.GetMeta()
	if err != nil {
		return "", errors.Wrap(err)
	}
	p, found := m.Annotations[kioutil.PathAnnotation]
	if !found {
		return "", nil
	}

	functionDir := path.Clean(path.Dir(p))

	if path.Base(functionDir) == functionsDirectoryName {
		// the scope of functions in a directory called "functions" is 1 level higher
		// this is similar to how the golang "internal" directory scoping works
		functionDir = path.Dir(functionDir)
	}
	return functionDir, nil
}

// scope partitions the input nodes into 2 slices.  The first slice contains only Resources
// which are scoped under dir, and the second slice contains the Resources which are not.
func (c *ContainerFilter) scope(dir string, nodes []*yaml.RNode) ([]*yaml.RNode, []*yaml.RNode, error) {
	// scope container filtered Resources to Resources under that directory
	var input, saved []*yaml.RNode
	if c.GlobalScope {
		return nodes, nil, nil
	}

	// global function
	if dir == "" || dir == "." {
		return nodes, nil, nil
	}

	// identify Resources read from directories under the function configuration
	for i := range nodes {
		m, err := nodes[i].GetMeta()
		if err != nil {
			return nil, nil, err
		}
		p, found := m.Annotations[kioutil.PathAnnotation]
		if !found {
			// this Resource isn't scoped under the function -- don't know where it came from
			// consider it out of scope
			saved = append(saved, nodes[i])
			continue
		}

		resourceDir := path.Clean(path.Dir(p))
		if path.Base(resourceDir) == functionsDirectoryName {
			// Functions in the `functions` directory are scoped to
			// themselves, and should see themselves as input
			resourceDir = path.Dir(resourceDir)
		}
		if !strings.HasPrefix(resourceDir, dir) {
			// this Resource doesn't fall under the function scope if it
			// isn't in a subdirectory of where the function lives
			saved = append(saved, nodes[i])
			continue
		}

		// this input is scoped under the function
		input = append(input, nodes[i])
	}

	return input, saved, nil
}

// GrepFilter implements kio.GrepFilter
func (c *ContainerFilter) Filter(nodes []*yaml.RNode) ([]*yaml.RNode, error) {
	// get the command to filter the Resources
	cmd := c.getCommand()

	in := &bytes.Buffer{}
	out := &bytes.Buffer{}

	// only process Resources scoped to this function, save the others
	functionDir, err := c.getFunctionScope()
	if err != nil {
		return nil, err
	}
	input, saved, err := c.scope(functionDir, nodes)
	if err != nil {
		return nil, err
	}

	// write the input
	err = kio.ByteWriter{
		WrappingAPIVersion:    kio.ResourceListAPIVersion,
		WrappingKind:          kio.ResourceListKind,
		Writer:                in,
		KeepReaderAnnotations: true,
		FunctionConfig:        c.Config}.Write(input)
	if err != nil {
		return nil, err
	}

	// capture the command stdout for the return value
	r := &kio.ByteReader{Reader: out}

	// do the filtering
	if c.checkInput != nil {
		c.checkInput(in.String())
	}
	cmd.Stdin = in
	cmd.Stdout = out

	// don't exit immediately if the function fails -- write out the validation
	c.Exit = cmd.Run()

	output, err := r.Read()
	if err != nil {
		return nil, err
	}

	if err := c.doResults(r); err != nil {
		return nil, err
	}

	if c.Exit != nil && !c.DeferFailure {
		return append(output, saved...), c.Exit
	}

	// annotate any generated Resources with a path and index if they don't already have one
	if err := kioutil.DefaultPathAnnotation(functionDir, output); err != nil {
		return nil, err
	}

	// emit both the Resources output from the function, and the out-of-scope Resources
	// which were not provided to the function
	return append(output, saved...), nil
}

func (c *ContainerFilter) doResults(r *kio.ByteReader) error {
	// Write the results to a file if configured to do so
	if c.ResultsFile != "" && r.Results != nil {
		results, err := r.Results.String()
		if err != nil {
			return err
		}
		err = ioutil.WriteFile(c.ResultsFile, []byte(results), 0600)
		if err != nil {
			return err
		}
	}

	if r.Results != nil {
		c.Results = r.Results
	}
	return nil
}

// getArgs returns the command + args to run to spawn the container
func (c *ContainerFilter) getArgs() []string {
	// run the container using docker.  this is simpler than using the docker
	// libraries, and ensures things like auth work the same as if the container
	// was run from the cli.

	network := "none"
	if c.Network != "" {
		network = c.Network
	}

	args := []string{"docker", "run",
		"--rm",                                              // delete the container afterward
		"-i", "-a", "STDIN", "-a", "STDOUT", "-a", "STDERR", // attach stdin, stdout, stderr

		// added security options
		"--network", network,
		"--user", "nobody", // run as nobody
		// don't make fs readonly because things like heredoc rely on writing tmp files
		"--security-opt=no-new-privileges", // don't allow the user to escalate privileges
	}

	// TODO(joncwong): Allow StorageMount fields to have default values.
	for _, storageMount := range c.StorageMounts {
		args = append(args, "--mount", storageMount.String())
	}

	// tell functions to write error messages to stderr as well as results
	os.Setenv("LOG_TO_STDERR", "true")
	os.Setenv("STRUCTURED_RESULTS", "true")

	// export the local environment vars to the container
	for _, pair := range os.Environ() {
		tokens := strings.Split(pair, "=")
		if tokens[0] == "" {
			continue
		}
		args = append(args, "-e", tokens[0])
	}
	return append(args, c.Image)
}

// getCommand returns a command which will apply the Filter using the container image
func (c *ContainerFilter) getCommand() *exec.Cmd {
	if c.SetFlowStyleForConfig {
		c.Config.YNode().Style = yaml.FlowStyle
	}

	if len(c.args) == 0 {
		c.args = c.getArgs()
	}

	cmd := exec.Command(c.args[0], c.args[1:]...)
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()

	// set stderr for err messaging
	return cmd
}

// IsReconcilerFilter filters Resources based on whether or not they are Reconciler Resource.
// Resources with an apiVersion starting with '*.gcr.io', 'gcr.io' or 'docker.io' are considered
// Reconciler Resources.
type IsReconcilerFilter struct {
	// ExcludeReconcilers if set to true, then Reconcilers will be excluded -- e.g.
	// Resources with a reconcile container through the apiVersion (gcr.io prefix) or
	// through the annotations
	ExcludeReconcilers bool `yaml:"excludeReconcilers,omitempty"`

	// IncludeNonReconcilers if set to true, the NonReconciler will be included.
	IncludeNonReconcilers bool `yaml:"includeNonReconcilers,omitempty"`
}

// Filter implements kio.Filter
func (c *IsReconcilerFilter) Filter(inputs []*yaml.RNode) ([]*yaml.RNode, error) {
	var out []*yaml.RNode
	for i := range inputs {
		isFnResource := GetFunctionSpec(inputs[i]) != nil
		if isFnResource && !c.ExcludeReconcilers {
			out = append(out, inputs[i])
		}
		if !isFnResource && c.IncludeNonReconcilers {
			out = append(out, inputs[i])
		}
	}
	return out, nil
}
