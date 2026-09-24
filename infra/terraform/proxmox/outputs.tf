# The contract with lib/setup-provision.sh: how to reach the Host over SSH.
output "host" {
  description = "The provisioned Host"
  value = {
    name      = module.vm.name
    vmid      = module.vm.vmid
    node      = module.vm.node_name
    ip        = module.vm.ipv4_ip
    user      = var.vm_user
    cores     = var.cores
    memory_mb = var.memory_mb
    disk_gb   = var.disk_gb
  }
}
