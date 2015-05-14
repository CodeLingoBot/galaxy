package config

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	docker "github.com/fsouza/go-dockerclient"
	"github.com/litl/galaxy/log"
)

func NewServiceRegistry(ttl uint64) *Store {
	return &Store{
		TTL:    ttl,
		pollCh: make(chan bool),
	}

}

func (r *Store) newServiceRegistration(container *docker.Container, hostIP, galaxyPort string) *ServiceRegistration {
	//FIXME: We're using the first found port and assuming it's tcp.
	//How should we handle a service that exposes multiple ports
	//as well as tcp vs udp ports.
	var externalPort, internalPort string

	// sort the port bindings by internal port number so multiple ports are assigned deterministically
	// (docker.Port is a string with a Port method)
	cPorts := container.NetworkSettings.Ports
	allPorts := []string{}
	for p, _ := range cPorts {
		allPorts = append(allPorts, string(p))
	}
	sort.Strings(allPorts)

	for _, k := range allPorts {
		v := cPorts[docker.Port(k)]
		if len(v) > 0 {
			externalPort = v[0].HostPort
			internalPort = docker.Port(k).Port()
			// Look for a match to GALAXY_PORT if we have multiple ports to
			// choose from. (don't require this, or we may break existing services)
			if len(allPorts) > 1 && internalPort == galaxyPort {
				break
			}
		}
	}

	serviceRegistration := ServiceRegistration{
		ContainerName: container.Name,
		ContainerID:   container.ID,
		StartedAt:     container.Created,
		Image:         container.Config.Image,
		Port:          galaxyPort,
	}

	if externalPort != "" && internalPort != "" {
		serviceRegistration.ExternalIP = hostIP
		serviceRegistration.InternalIP = container.NetworkSettings.IPAddress
		serviceRegistration.ExternalPort = externalPort
		serviceRegistration.InternalPort = internalPort
	}
	return &serviceRegistration
}

type ServiceRegistration struct {
	Name          string            `json:"NAME,omitempty"`
	ExternalIP    string            `json:"EXTERNAL_IP,omitempty"`
	ExternalPort  string            `json:"EXTERNAL_PORT,omitempty"`
	InternalIP    string            `json:"INTERNAL_IP,omitempty"`
	InternalPort  string            `json:"INTERNAL_PORT,omitempty"`
	ContainerID   string            `json:"CONTAINER_ID"`
	ContainerName string            `json:"CONTAINER_NAME"`
	Image         string            `json:"IMAGE,omitempty"`
	ImageId       string            `json:"IMAGE_ID,omitempty"`
	StartedAt     time.Time         `json:"STARTED_AT"`
	Expires       time.Time         `json:"-"`
	Path          string            `json:"-"`
	VirtualHosts  []string          `json:"VIRTUAL_HOSTS"`
	Port          string            `json:"PORT"`
	ErrorPages    map[string]string `json:"ERROR_PAGES,omitempty"`
}

func (s *ServiceRegistration) Equals(other ServiceRegistration) bool {
	return s.ExternalIP == other.ExternalIP &&
		s.ExternalPort == other.ExternalPort &&
		s.InternalIP == other.InternalIP &&
		s.InternalPort == other.InternalPort
}

func (s *ServiceRegistration) addr(ip, port string) string {
	if ip != "" && port != "" {
		return fmt.Sprint(ip, ":", port)
	}
	return ""

}
func (s *ServiceRegistration) ExternalAddr() string {
	return s.addr(s.ExternalIP, s.ExternalPort)
}

func (s *ServiceRegistration) InternalAddr() string {
	return s.addr(s.InternalIP, s.InternalPort)
}

func (r *Store) RegisterService(env, pool, hostIP string, container *docker.Container) (*ServiceRegistration, error) {
	environment := r.EnvFor(container)

	name := environment["GALAXY_APP"]
	if name == "" {
		return nil, fmt.Errorf("GALAXY_APP not set on container %s", container.ID[0:12])
	}

	registrationPath := path.Join(env, pool, "hosts", hostIP, name, container.ID[0:12])

	serviceRegistration := r.newServiceRegistration(container, hostIP, environment["GALAXY_PORT"])
	serviceRegistration.Name = name
	serviceRegistration.ImageId = container.Config.Image

	vhosts := environment["VIRTUAL_HOST"]
	if strings.TrimSpace(vhosts) != "" {
		serviceRegistration.VirtualHosts = strings.Split(vhosts, ",")
	}

	errorPages := make(map[string]string)

	// scan environment variables for the VIRTUAL_HOST_%d pattern
	// but save the original variable and url.
	for vhostCode, url := range environment {
		code := 0
		n, err := fmt.Sscanf(vhostCode, "VIRTUAL_HOST_%d", &code)
		if err != nil || n == 0 {
			continue
		}

		errorPages[vhostCode] = url
	}

	if len(errorPages) > 0 {
		serviceRegistration.ErrorPages = errorPages
	}

	jsonReg, err := json.Marshal(serviceRegistration)
	if err != nil {
		return nil, err
	}

	// TODO: use a compare-and-swap SCRIPT
	_, err = r.Backend.Set(registrationPath, "location", string(jsonReg))
	if err != nil {
		return nil, err
	}

	_, err = r.Backend.Expire(registrationPath, r.TTL)
	if err != nil {
		return nil, err
	}
	serviceRegistration.Expires = time.Now().UTC().Add(time.Duration(r.TTL) * time.Second)

	return serviceRegistration, nil
}

func (r *Store) UnRegisterService(env, pool, hostIP string, container *docker.Container) (*ServiceRegistration, error) {

	environment := r.EnvFor(container)

	name := environment["GALAXY_APP"]
	if name == "" {
		return nil, fmt.Errorf("GALAXY_APP not set on container %s", container.ID[0:12])
	}

	registrationPath := path.Join(env, pool, "hosts", hostIP, name, container.ID[0:12])

	registration, err := r.GetServiceRegistration(env, pool, hostIP, container)
	if err != nil || registration == nil {
		return registration, err
	}

	if registration.ContainerID != container.ID {
		return nil, nil
	}

	_, err = r.Backend.Delete(registrationPath)
	if err != nil {
		return registration, err
	}

	return registration, nil
}

func (r *Store) GetServiceRegistration(env, pool, hostIP string, container *docker.Container) (*ServiceRegistration, error) {

	environment := r.EnvFor(container)

	name := environment["GALAXY_APP"]
	if name == "" {
		return nil, fmt.Errorf("GALAXY_APP not set on container %s", container.ID[0:12])
	}

	regPath := path.Join(env, pool, "hosts", hostIP, name, container.ID[0:12])

	existingRegistration := ServiceRegistration{
		Path: regPath,
	}

	location, err := r.Backend.Get(regPath, "location")

	if err != nil {
		return nil, err
	}

	if location != "" {
		err = json.Unmarshal([]byte(location), &existingRegistration)
		if err != nil {
			return nil, err
		}

		expires, err := r.Backend.TTL(regPath)
		if err != nil {
			return nil, err
		}
		existingRegistration.Expires = time.Now().UTC().Add(time.Duration(expires) * time.Second)
		return &existingRegistration, nil
	}

	return nil, nil
}

func (r *Store) IsRegistered(env, pool, hostIP string, container *docker.Container) (bool, error) {

	reg, err := r.GetServiceRegistration(env, pool, hostIP, container)
	return reg != nil, err
}

// TODO: get all ServiceRegistrations
func (r *Store) ListRegistrations(env string) ([]ServiceRegistration, error) {

	// TODO: convert to scan
	keys, err := r.Backend.Keys(path.Join(env, "*", "hosts", "*", "*", "*"))
	if err != nil {
		return nil, err
	}

	var regList []ServiceRegistration
	for _, key := range keys {

		val, err := r.Backend.Get(key, "location")
		if err != nil {
			log.Warnf("WARN: Unable to get location for %s: %s", key, err)
			continue
		}

		svcReg := ServiceRegistration{
			Name: path.Base(key),
		}
		err = json.Unmarshal([]byte(val), &svcReg)
		if err != nil {
			log.Warnf("WARN: Unable to unmarshal JSON for %s: %s", key, err)
			continue
		}

		svcReg.Path = key

		regList = append(regList, svcReg)
	}

	return regList, nil
}

func (s *Store) EnvFor(container *docker.Container) map[string]string {
	env := map[string]string{}
	for _, item := range container.Config.Env {
		sep := strings.Index(item, "=")
		k := item[0:sep]
		v := item[sep+1:]
		env[k] = v
	}
	return env
}
