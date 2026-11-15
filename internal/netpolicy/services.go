package netpolicy

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/JCombee/wsm/internal/projectuid"
)

// ServiceSet is the set of grants: uid . address . port (ADR 0013).
const ServiceSet = "svc_allow4"

// checkGrant validates the numbers of a grant.
func checkGrant(wsm0 netip.Addr, uid, port int) error {
	if !projectuid.Valid(uid) {
		return fmt.Errorf("netpolicy: uid %d is outside %d-%d", uid, projectuid.Min, projectuid.Max)
	}
	if !ValidServicePort(port) {
		return fmt.Errorf("netpolicy: port %d is outside %d-%d", port, ServicePortMin, ServicePortMax)
	}
	if !wsm0.Is4() {
		return fmt.Errorf("netpolicy: wsm0 address %v is not IPv4", wsm0)
	}
	return nil
}

func element(op string, wsm0 netip.Addr, uid, port int) string {
	return fmt.Sprintf("%s element %s %s { %d . %s . %d }\n", op, Table, ServiceSet, uid, wsm0, port)
}

// AllowServiceScript returns an nft script that lets uid reach port on
// wsm0. Adding an element twice leaves one element.
func AllowServiceScript(wsm0 netip.Addr, uid, port int) (string, error) {
	if err := checkGrant(wsm0, uid, port); err != nil {
		return "", err
	}
	return element("add", wsm0, uid, port), nil
}

// RevokeServiceScript returns an nft script that removes that grant.
// "destroy" does not fail on a missing element.
func RevokeServiceScript(wsm0 netip.Addr, uid, port int) (string, error) {
	if err := checkGrant(wsm0, uid, port); err != nil {
		return "", err
	}
	return element("destroy", wsm0, uid, port), nil
}

// RevokeAllServicesScript returns an nft script that removes every grant of
// uid, whatever the port, and leaves the grants of other UIDs alone. It
// names each port of the range, so it needs no listing of the set.
func RevokeAllServicesScript(wsm0 netip.Addr, uid int) (string, error) {
	if err := checkGrant(wsm0, uid, ServicePortMin); err != nil {
		return "", err
	}
	var b strings.Builder
	for port := ServicePortMin; port <= ServicePortMax; port++ {
		b.WriteString(element("destroy", wsm0, uid, port))
	}
	return b.String(), nil
}
