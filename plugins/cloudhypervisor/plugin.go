package cloudhypervisor

import "voidrun/pkg/compute"

func init() {
	compute.Register(string(compute.TypeCloudHypervisor), func() compute.Hypervisor {
		return &Provider{}
	})
}

// Provider implements compute.Hypervisor for Cloud Hypervisor.
type Provider struct{}

func (p *Provider) Name() string { return string(compute.TypeCloudHypervisor) }
