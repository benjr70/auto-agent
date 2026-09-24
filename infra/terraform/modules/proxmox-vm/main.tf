# One Proxmox VM booted from a cloud image with cloud-init. Ported from
# Smart-Smoker-V2's arm64-vm module: the same bpg resource and knobs, with the
# template clone replaced by an imported cloud image disk and the cloud-init
# password dropped (the operator's SSH key is the only way in, so nothing
# secret reaches terraform state).
terraform {
  required_providers {
    proxmox = {
      source = "bpg/proxmox"
    }
  }
}

locals {
  pool_id     = trimspace(var.resource_pool) == "" ? null : var.resource_pool
  dns_domain  = trimspace(var.search_domain) == "" ? null : var.search_domain
  mac_address = trimspace(var.mac_address) == "" ? null : var.mac_address
  vlan_id     = var.vlan_tag > 0 ? var.vlan_tag : null
}

resource "proxmox_virtual_environment_vm" "this" {
  node_name   = var.target_node
  vm_id       = var.vm_id
  name        = var.vm_name
  description = var.description
  pool_id     = local.pool_id
  tags        = var.tags
  on_boot     = var.onboot

  machine = var.machine_type

  agent {
    enabled = var.enable_qemu_agent
  }

  cpu {
    cores   = var.cpu_cores
    sockets = var.cpu_sockets
    type    = var.cpu_type
  }

  memory {
    dedicated = var.memory_mb
  }

  disk {
    datastore_id = var.storage
    file_id      = var.image_file_id
    interface    = "virtio0"
    size         = var.disk_gb
    discard      = "on"
    iothread     = true
  }

  network_device {
    bridge      = var.network_bridge
    firewall    = var.enable_firewall
    mac_address = local.mac_address
    vlan_id     = local.vlan_id
  }

  operating_system {
    type = var.os_type
  }

  # cloud-init: Ubuntu's default user renamed to cloud_init_user keeps its
  # passwordless sudo; the key is its only credential.
  initialization {
    datastore_id = var.storage

    ip_config {
      ipv4 {
        address = var.ipv4_cidr
        gateway = var.gateway
      }
    }

    dns {
      domain  = local.dns_domain
      servers = var.dns_servers
    }

    user_account {
      username = var.cloud_init_user
      keys     = var.ssh_public_keys
    }
  }

  # Cloud images log to the serial console.
  serial_device {}

  vga {
    type = "serial0"
  }
}
