variable "zone_id" {
  description = "Cloudflare zone ID."
  type        = string
}

variable "zone_name" {
  description = "Cloudflare zone name."
  type        = string
}

variable "app_subdomain" {
  description = "Dashboard subdomain relative to zone."
  type        = string
}

variable "api_subdomain" {
  description = "API subdomain relative to zone."
  type        = string
}

variable "www_subdomain" {
  description = "Marketing www subdomain (empty to skip)."
  type        = string
  default     = ""
}

variable "eip_address" {
  description = "Elastic IP for A records."
  type        = string
}
