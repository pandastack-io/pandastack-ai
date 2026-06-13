resource "cloudflare_record" "app" {
  zone_id = var.zone_id
  name    = var.app_subdomain
  type    = "A"
  value   = var.eip_address
  proxied = true
  ttl     = 1
}

resource "cloudflare_record" "api" {
  zone_id = var.zone_id
  name    = var.api_subdomain
  type    = "A"
  value   = var.eip_address
  proxied = true
  ttl     = 1
}

resource "cloudflare_record" "www" {
  count   = var.www_subdomain == "" ? 0 : 1
  zone_id = var.zone_id
  name    = var.www_subdomain
  type    = "A"
  value   = var.eip_address
  proxied = true
  ttl     = 1
}
