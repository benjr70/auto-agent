# The Proxmox Provisioner (ADR 0004): one Host VM from an Ubuntu Server cloud
# image, first-booted by cloud-init with the operator's SSH key. Setup then
# reaches it over SSH like a brought VM, so everything after provisioning is
# the shared configure step. A second Provisioner is a sibling environment
# with the same outputs.

resource "proxmox_download_file" "image" {
  node_name    = var.node
  datastore_id = var.image_datastore
  content_type = "iso"
  url          = var.image_url
  # One image per Host, so destroying one Host's state never deletes the file
  # another Host's state manages.
  file_name = "auto-agent-${var.name}-${basename(var.image_url)}"

  # Converge: a re-run never re-downloads because upstream published a new
  # build (overwrite = false), and a file of this name left by a lost state is
  # replaced rather than failing. The VM's disk is a copy, so the file never
  # touches a running Host.
  overwrite           = false
  overwrite_unmanaged = true
}

module "vm" {
  source = "../modules/proxmox-vm"

  vm_name         = var.name
  vm_id           = var.vm_id
  description     = "auto-agent Host ${var.name}, provisioned by bin/auto-agent setup --provision proxmox"
  target_node     = var.node
  image_file_id   = proxmox_download_file.image.id
  storage         = var.datastore
  disk_gb         = var.disk_gb
  cpu_cores       = var.cores
  memory_mb       = var.memory_mb
  network_bridge  = var.bridge
  vlan_tag        = var.vlan_tag
  ipv4_cidr       = var.ipv4_cidr
  gateway         = var.gateway
  dns_servers     = var.dns_servers
  cloud_init_user = var.vm_user
  ssh_public_keys = var.ssh_public_keys
  tags            = var.tags
}
