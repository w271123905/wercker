//   Copyright 2016 Wercker Holding BV
//
//   Licensed under the Apache License, Version 2.0 (the "License");
//   you may not use this file except in compliance with the License.
//   You may obtain a copy of the License at
//
//       http://www.apache.org/licenses/LICENSE-2.0
//
//   Unless required by applicable law or agreed to in writing, software
//   distributed under the License is distributed on an "AS IS" BASIS,
//   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//   See the License for the specific language governing permissions and
//   limitations under the License.

package dockerlocal

import (
	"fmt"
	"io"
	"io/ioutil"
	"net/url"
	"os"
	"strings"

	"github.com/fsouza/go-dockerclient"
	"github.com/google/shlex"
	"github.com/wercker/wercker/auth"
	"github.com/wercker/wercker/core"
	"github.com/wercker/wercker/util"

	"golang.org/x/net/context"
)

// TODO(termie): remove references to docker

// Box is our wrapper for Box operations
type DockerBox struct {
	Name            string
	ShortName       string
	networkDisabled bool
	client          *DockerClient
	services        []core.ServiceBox
	options         *core.PipelineOptions
	dockerOptions   *Options
	container       *docker.Container
	config          *core.BoxConfig
	cmd             string
	repository      string
	tag             string
	images          []*docker.Image
	logger          *util.LogEntry
	entrypoint      string
	image           *docker.Image
	volumes         []string
}

// NewDockerBox from a name and other references
func NewDockerBox(boxConfig *core.BoxConfig, options *core.PipelineOptions, dockerOptions *Options) (*DockerBox, error) {
	name := boxConfig.ID

	if strings.Contains(name, "@") {
		return nil, fmt.Errorf("Invalid box name, '@' is not allowed in docker repositories.")
	}

	parts := strings.Split(name, ":")
	repository := parts[0]
	tag := "latest"
	if len(parts) > 1 {
		tag = parts[1]
	}
	if boxConfig.Tag != "" {
		tag = boxConfig.Tag
	}
	// checkpoint support
	if options.Checkpoint != "" {
		tag = fmt.Sprintf("w-%s", options.Checkpoint)
	}
	name = fmt.Sprintf("%s:%s", repository, tag)

	repoParts := strings.Split(repository, "/")
	shortName := repository
	if len(repoParts) > 1 {
		shortName = repoParts[len(repoParts)-1]
	}

	networkDisabled := false

	cmd := boxConfig.Cmd
	if cmd == "" {
		cmd = DefaultDockerCommand
	}

	entrypoint := boxConfig.Entrypoint

	logger := util.RootLogger().WithFields(util.LogFields{
		"Logger":    "Box",
		"Name":      name,
		"ShortName": shortName,
	})

	client, err := NewDockerClient(dockerOptions)
	if err != nil {
		return nil, err
	}
	return &DockerBox{
		Name:            name,
		ShortName:       shortName,
		client:          client,
		config:          boxConfig,
		options:         options,
		dockerOptions:   dockerOptions,
		repository:      repository,
		tag:             tag,
		networkDisabled: networkDisabled,
		logger:          logger,
		cmd:             cmd,
		entrypoint:      entrypoint,
		volumes:         []string{},
	}, nil
}

func (b *DockerBox) links() []string {
	serviceLinks := []string{}

	for _, service := range b.services {
		serviceLinks = append(serviceLinks, service.Link())
	}
	b.logger.Debugln("Creating links:", serviceLinks)
	return serviceLinks
}

// Link gives us the parameter to Docker to link to this box
func (b *DockerBox) Link() string {
	name := b.config.Name
	if name == "" {
		name = b.ShortName
	}
	return fmt.Sprintf("%s:%s", b.container.Name, name)
}

// GetName gets the box name
func (b *DockerBox) GetName() string {
	return b.Name
}

func (b *DockerBox) Repository() string {
	return b.repository
}

func (b *DockerBox) GetTag() string {
	return b.tag
}

// GetID gets the container ID or empty string if we don't have a container
func (b *DockerBox) GetID() string {
	if b.container != nil {
		return b.container.ID
	}
	return ""
}

func (b *DockerBox) binds(env *util.Environment) ([]string, error) {
	binds := []string{}
	// Make our list of binds for the Docker attach
	// NOTE(termie): we don't appear to need the "volumes" stuff, leaving
	//               it commented out in case it actually does something
	// volumes := make(map[string]struct{})
	entries, err := ioutil.ReadDir(b.options.HostPath())
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Mode()&os.ModeSymlink == os.ModeSymlink {

			// For local dev we can mount read-write and avoid a copy, so we'll mount
			// directly in the pipeline path
			if b.options.DirectMount {
				binds = append(binds, fmt.Sprintf("%s:%s:rw", b.options.HostPath(entry.Name()), b.options.GuestPath(entry.Name())))
			} else {
				binds = append(binds, fmt.Sprintf("%s:%s:ro", b.options.HostPath(entry.Name()), b.options.MntPath(entry.Name())))
			}
			// volumes[b.options.MntPath(entry.Name())] = struct{}{}
		}
	}

	if b.options.EnableVolumes {
		vols := util.SplitSpaceOrComma(b.config.Volumes)
		var interpolatedVols []string
		for _, vol := range vols {
			if strings.Contains(vol, ":") {
				pair := strings.SplitN(vol, ":", 2)
				interpolatedVols = append(interpolatedVols, env.Interpolate(pair[0]))
				interpolatedVols = append(interpolatedVols, env.Interpolate(pair[1]))
			} else {
				interpolatedVols = append(interpolatedVols, env.Interpolate(vol))
				interpolatedVols = append(interpolatedVols, env.Interpolate(vol))
			}
		}
		b.volumes = interpolatedVols
		for i := 0; i < len(b.volumes); i += 2 {
			binds = append(binds, fmt.Sprintf("%s:%s:rw", b.volumes[i], b.volumes[i+1]))
		}
	}

	return binds, nil
}

// RunServices runs the services associated with this box
func (b *DockerBox) RunServices(ctx context.Context, env *util.Environment) error {
	links := []string{}

	// TODO(termie): terrible hack, sorry world
	ctxWithServiceCount := context.WithValue(ctx, "ServiceCount", len(b.services))

	for _, service := range b.services {
		b.logger.Debugln("Startinq service:", service.GetName())
		_, err := service.Run(ctxWithServiceCount, env, links)
		if err != nil {
			return err
		}
		links = append(links, service.Link())
	}
	return nil
}

func dockerEnv(boxEnv map[string]string, env *util.Environment) []string {
	s := []string{}
	for k, v := range boxEnv {
		s = append(s, fmt.Sprintf("%s=%s", strings.ToUpper(k), env.Interpolate(v)))
	}
	return s
}

func portBindings(published []string) map[docker.Port][]docker.PortBinding {
	outer := make(map[docker.Port][]docker.PortBinding)
	for _, portdef := range published {
		var ip string
		var hostPort string
		var containerPort string

		parts := strings.Split(portdef, ":")

		switch {
		case len(parts) == 3:
			ip = parts[0]
			hostPort = parts[1]
			containerPort = parts[2]
		case len(parts) == 2:
			hostPort = parts[0]
			containerPort = parts[1]
		case len(parts) == 1:
			hostPort = parts[0]
			containerPort = parts[0]
		}
		// Make sure we have a protocol in the container port
		if !strings.Contains(containerPort, "/") {
			containerPort = containerPort + "/tcp"
		}

		if hostPort == "" {
			hostPort = containerPort
		}

		// Just in case we have a /tcp in there
		hostParts := strings.Split(hostPort, "/")
		hostPort = hostParts[0]
		portBinding := docker.PortBinding{
			HostPort: hostPort,
		}
		if ip != "" {
			portBinding.HostIP = ip
		}
		outer[docker.Port(containerPort)] = []docker.PortBinding{portBinding}
	}
	return outer
}

func exposedPorts(published []string) map[docker.Port]struct{} {
	portBinds := portBindings(published)
	exposed := make(map[docker.Port]struct{})
	for port := range portBinds {
		exposed[port] = struct{}{}
	}
	return exposed
}

// ExposedPortMap contains port forwarding information
type ExposedPortMap struct {
	ContainerPort string
	HostURI       string
}

// exposedPortMaps returns a list of exposed ports and the host
func exposedPortMaps(dockerHost string, published []string) ([]ExposedPortMap, error) {
	if dockerHost != "" {
		docker, err := url.Parse(dockerHost)
		if err != nil {
			return nil, err
		}
		if docker.Scheme == "unix" {
			dockerHost = "localhost"
		} else {
			dockerHost = strings.Split(docker.Host, ":")[0]
		}
	}
	portMap := []ExposedPortMap{}
	for k, v := range portBindings(published) {
		for _, port := range v {
			p := ExposedPortMap{
				ContainerPort: k.Port(),
				HostURI:       fmt.Sprintf("%s:%s", dockerHost, port.HostPort),
			}
			portMap = append(portMap, p)
		}
	}
	return portMap, nil
}

//RecoverInteractive restarts the box with a terminal attached
func (b *DockerBox) RecoverInteractive(cwd string, pipeline core.Pipeline, step core.Step) error {
	// TODO(termie): maybe move the container manipulation outside of here?
	client := b.client
	container, err := b.Restart()
	if err != nil {
		b.logger.Panicln("box restart failed")
		return err
	}

	env := []string{}
	env = append(env, pipeline.Env().Export()...)
	env = append(env, pipeline.Env().Hidden.Export()...)
	env = append(env, step.Env().Export()...)
	env = append(env, fmt.Sprintf("cd %s", cwd))
	cmd, err := shlex.Split(b.cmd)
	if err != nil {
		return err
	}
	return client.AttachInteractive(container.ID, cmd, env)
}

func (b *DockerBox) getContainerName() string {
	return "wercker-pipeline-" + b.options.RunID
}

// Run creates the container and runs it.
func (b *DockerBox) Run(ctx context.Context, env *util.Environment) (*docker.Container, error) {
	err := b.RunServices(ctx, env)
	if err != nil {
		return nil, err
	}
	b.logger.Debugln("Starting base box:", b.Name)

	// TODO(termie): maybe move the container manipulation outside of here?
	client := b.client

	// Import the environment
	myEnv := dockerEnv(b.config.Env, env)

	var entrypoint []string
	if b.entrypoint != "" {
		entrypoint, err = shlex.Split(b.entrypoint)
		if err != nil {
			return nil, err
		}
	}

	cmd, err := shlex.Split(b.cmd)
	if err != nil {
		return nil, err
	}

	var ports map[docker.Port]struct{}
	if len(b.options.PublishPorts) > 0 {
		ports = exposedPorts(b.options.PublishPorts)
	} else if b.options.ExposePorts {
		ports = exposedPorts(b.config.Ports)
	}

	binds, err := b.binds(env)

	portsToBind := []string{""}

	if len(b.options.PublishPorts) >= 1 {
		b.logger.Warnln("--publish is deprecated, please use --expose-ports and define the ports for the boxes. See: https://github.com/wercker/wercker/pull/161")
		portsToBind = b.options.PublishPorts
	} else if b.options.ExposePorts {
		portsToBind = b.config.Ports
	}

	hostConfig := &docker.HostConfig{
		Binds:        binds,
		Links:        b.links(),
		PortBindings: portBindings(portsToBind),
		DNS:          b.dockerOptions.DNS,
	}

	conf := &docker.Config{
		Image:           env.Interpolate(b.Name),
		Tty:             false,
		OpenStdin:       true,
		Cmd:             cmd,
		Env:             myEnv,
		AttachStdin:     true,
		AttachStdout:    true,
		AttachStderr:    true,
		ExposedPorts:    ports,
		NetworkDisabled: b.networkDisabled,
		DNS:             b.dockerOptions.DNS,
		Entrypoint:      entrypoint,
		// Volumes: volumes,
	}

	if b.dockerOptions.Memory != 0 {
		mem := b.dockerOptions.Memory
		if len(b.services) > 0 {
			mem = int64(float64(mem) * 0.75)
		}
		swap := b.dockerOptions.MemorySwap
		if swap == 0 {
			swap = 2 * mem
		}

		conf.Memory = mem
		conf.MemorySwap = swap
	}

	// Make and start the container
	container, err := client.CreateContainer(
		docker.CreateContainerOptions{
			Name:       b.getContainerName(),
			Config:     conf,
			HostConfig: hostConfig,
		})

	if err != nil {
		return nil, err
	}

	b.logger.Debugln("Docker Container:", container.ID)

	if err != nil {
		return nil, err
	}

	client.StartContainer(container.ID, hostConfig)
	b.container = container
	return container, nil
}

// Clean up the containers
func (b *DockerBox) Clean() error {
	containers := []string{}
	if b.container != nil {
		containers = append(containers, b.container.ID)
	}

	for _, service := range b.services {
		if containerID := service.GetID(); containerID != "" {
			containers = append(containers, containerID)
		}
	}

	// TODO(termie): maybe move the container manipulation outside of here?
	client := b.client

	for _, container := range containers {
		opts := docker.RemoveContainerOptions{
			ID: container,
			// God, if you exist, thank you for removing these containers,
			// that their biological and cultural diversity is not added
			// to our own but is expunged from us with fiery vengeance.
			RemoveVolumes: true,
			Force:         true,
		}
		b.logger.WithField("Container", container).Debugln("Removing container:", container)
		err := client.RemoveContainer(opts)
		if err != nil {
			return err
		}
	}

	if !b.options.ShouldCommit {
		for i := len(b.images) - 1; i >= 0; i-- {
			b.logger.WithField("Image", b.images[i].ID).Debugln("Removing image:", b.images[i].ID)
			client.RemoveImage(b.images[i].ID)
		}
	}

	return nil
}

// Restart stops and starts the box
func (b *DockerBox) Restart() (*docker.Container, error) {
	// TODO(termie): maybe move the container manipulation outside of here?
	client := b.client
	err := client.RestartContainer(b.container.ID, 1)
	if err != nil {
		return nil, err
	}
	return b.container, nil
}

// AddService needed by this Box
func (b *DockerBox) AddService(service core.ServiceBox) {
	b.services = append(b.services, service)
}

// Stop the box and all its services
func (b *DockerBox) Stop() {
	// TODO(termie): maybe move the container manipulation outside of here?
	client := b.client
	for _, service := range b.services {
		b.logger.Debugln("Stopping service", service.GetID())
		err := client.StopContainer(service.GetID(), 1)

		if err != nil {
			if _, ok := err.(*docker.ContainerNotRunning); ok {
				b.logger.Warnln("Service container has already stopped.")
			} else {
				b.logger.WithField("Error", err).Warnln("Wasn't able to stop service container", service.GetID())
			}
		}
	}
	if b.container != nil {
		b.logger.Debugln("Stopping container", b.container.ID)
		err := client.StopContainer(b.container.ID, 1)

		if err != nil {
			if _, ok := err.(*docker.ContainerNotRunning); ok {
				b.logger.Warnln("Box container has already stopped.")
			} else {
				b.logger.WithField("Error", err).Warnln("Wasn't able to stop box container", b.container.ID)
			}
		}
	}
}

// Fetch an image (or update the local)
func (b *DockerBox) Fetch(ctx context.Context, env *util.Environment) (*docker.Image, error) {
	// TODO(termie): maybe move the container manipulation outside of here?
	client := b.client

	e, err := core.EmitterFromContext(ctx)
	if err != nil {
		return nil, err
	}
	repo := env.Interpolate(b.repository)

	b.config.Auth.Interpolate(env)

	// If user use Azure or AWS container registry we don't infer.
	if b.config.Auth.AzureClientSecret == "" && b.config.Auth.AwsSecretKey == "" {
		repo, b.config.Auth = InferRegistry(repo, b.config.Auth, b.options)
	}

	if b.config.Auth.Registry == b.options.WerckerContainerRegistry.String() {
		b.config.Auth.Username = DefaultDockerRegistryUsername
		b.config.Auth.Password = b.options.AuthToken
	}

	authenticator, err := dockerauth.GetRegistryAuthenticator(b.config.Auth)
	if err != nil {
		return nil, err
	}

	b.repository = authenticator.Repository(repo)
	b.Name = fmt.Sprintf("%s:%s", b.repository, b.tag)
	// Shortcut to speed up local dev
	if b.dockerOptions.Local {
		image, err := client.InspectImage(env.Interpolate(b.Name))
		if err != nil {
			return nil, err
		}
		b.image = image
		return image, nil
	}

	// Create a pipe since we want a io.Reader but Docker expects a io.Writer
	r, w := io.Pipe()
	defer w.Close()

	// emitStatusses in a different go routine
	go EmitStatus(e, r, b.options)

	options := docker.PullImageOptions{
		OutputStream:  w,
		RawJSONStream: true,
		Repository:    b.repository,
		Tag:           env.Interpolate(b.tag),
	}
	authConfig := docker.AuthConfiguration{
		Username: authenticator.Username(),
		Password: authenticator.Password(),
	}
	err = client.PullImage(options, authConfig)
	if err != nil {
		return nil, err
	}
	image, err := client.InspectImage(env.Interpolate(b.Name))
	if err != nil {
		return nil, err
	}
	b.image = image

	return nil, err
}

// Commit the current running Docker container to an Docker image.
func (b *DockerBox) Commit(name, tag, message string, cleanup bool) (*docker.Image, error) {
	b.logger.WithFields(util.LogFields{
		"Name": name,
		"Tag":  tag,
	}).Debugln("Commit container:", name, tag)

	// TODO(termie): maybe move the container manipulation outside of here?
	client := b.client

	commitOptions := docker.CommitContainerOptions{
		Container:  b.container.ID,
		Repository: name,
		Tag:        tag,
		Message:    "Build completed",
		Author:     "wercker",
	}
	image, err := client.CommitContainer(commitOptions)
	if err != nil {
		return nil, err
	}

	if cleanup {
		b.images = append(b.images, image)
	}

	return image, nil
}

// ExportImageOptions are the options available for ExportImage.
type ExportImageOptions struct {
	Name         string
	OutputStream io.Writer
}

// ExportImage will export the image to a temporary file and return the path to
// the file.
func (b *DockerBox) ExportImage(options *ExportImageOptions) error {
	b.logger.WithField("ExportName", options.Name).Info("Storing image")

	exportImageOptions := docker.ExportImageOptions{
		Name:         options.Name,
		OutputStream: options.OutputStream,
	}

	// TODO(termie): maybe move the container manipulation outside of here?
	client := b.client

	return client.ExportImage(exportImageOptions)
}
