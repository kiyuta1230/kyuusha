package kyuusha.network.quota

import rego.v1

# See docs/architecture.md "Quota設計": the limit values (QuotaSpec) live in
# identity, but the used+requested<=max judgement itself runs here, in
# network, against usage network tracks locally -- same reasoning as
# internal/authz and internal/compute/quota.rego.
#
# Subnet and NetworkInterface each get their own rule (not one shared
# "allow" with a kind parameter) -- same "different Create shapes get
# sibling rules in one package" convention as compute's allow/allow_resize.

default allow_subnet := false

allow_subnet if {
	input.usage.subnet_count + 1 <= input.limit.max_subnets
}

default allow_network_interface := false

allow_network_interface if {
	input.usage.network_interface_count + 1 <= input.limit.max_network_interfaces
}

default allow_ip_reservation := false

allow_ip_reservation if {
	input.usage.ip_reservation_count + 1 <= input.limit.max_ip_reservations
}
