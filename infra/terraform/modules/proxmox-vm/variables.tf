variable "vm_name" {
  description = "VM name"
  type        = string
}

variable "vm_id" {
  description = "Optional static VMID"
  type        = number
  default     = null
}

variable "description" {
  description = "Optional description"
  type        = string
  default     = ""
}

variable "target_node" {
  description = "Proxmox node to run the VM"
  type        = string
}

variable "resource_pool" {
  description = "Optional resource pool"
  type        = string
  default     = ""
}

variable "image_file_id" {
  description = "Cloud image the boot disk is imported from (<datastore>:iso/<file>)"
  type        = string
}

variable "onboot" {
  description = "Start VM on host boot"
  type        = bool
  default     = true
}

variable "enable_qemu_agent" {
  description = "Enable the QEMU guest agent (the stock cloud image does not ship one)"
  type        = bool
  default     = false
}

variable "cpu_sockets" {
  description = "Number of CPU sockets"
  type        = number
  default     = 1
}

variable "cpu_cores" {
  description = "Number of cores"
  type        = number
}

variable "cpu_type" {
  description = "CPU type definition"
  type        = string
  default     = "host"
}

variable "memory_mb" {
  description = "Memory allocation in MB"
  type        = number
}

variable "machine_type" {
  description = "QEMU machine type"
  type        = string
  default     = "q35"
}

variable "storage" {
  description = "Datastore for the boot disk and the cloud-init drive"
  type        = string
}

variable "disk_gb" {
  description = "Boot disk size in GB (the image is grown to it on first boot)"
  type        = number
}

variable "network_bridge" {
  description = "Network bridge"
  type        = string
  default     = "vmbr0"
}

variable "enable_firewall" {
  description = "Enable firewall for NIC"
  type        = bool
  default     = false
}

variable "vlan_tag" {
  description = "Optional VLAN tag"
  type        = number
  default     = 0
}

variable "mac_address" {
  description = "Optional MAC address"
  type        = string
  default     = ""
}

variable "os_type" {
  description = "Guest OS type"
  type        = string
  default     = "l26"
}

variable "cloud_init_user" {
  description = "The login cloud-init creates (Ubuntu's default user, renamed)"
  type        = string
}

variable "ssh_public_keys" {
  description = "SSH public keys for cloud_init_user"
  type        = list(string)
}

variable "ipv4_cidr" {
  description = "VM IPv4 CIDR"
  type        = string
}

variable "gateway" {
  description = "Default gateway"
  type        = string
}

variable "dns_servers" {
  description = "DNS servers"
  type        = list(string)
  default     = []
}

variable "search_domain" {
  description = "DNS search domain"
  type        = string
  default     = ""
}

variable "tags" {
  description = "Optional Proxmox tags"
  type        = list(string)
  default     = []
}
