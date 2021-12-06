package cloud_agent

import (
	"encoding/json"
	"github.com/bep/debounce"
	"github.com/fabedge/fabedge/pkg/common/about"
	"github.com/fabedge/fabedge/pkg/common/constants"
	"github.com/fabedge/fabedge/pkg/connector/routing"
	logutil "github.com/fabedge/fabedge/pkg/util/log"
	"github.com/fabedge/fabedge/pkg/util/memberlist"
	routeUtil "github.com/fabedge/fabedge/pkg/util/route"
	flag "github.com/spf13/pflag"
	"github.com/vishvananda/netlink"
	"k8s.io/klog/v2"
	"net"
	"time"
)

var (
	initMembers []string
	debounced   = debounce.New(time.Second * 10)
	addedRoutes = map[string][]netlink.Route{}
)

func init() {
	logutil.AddFlags(flag.CommandLine)
	flag.StringSliceVar(&initMembers, "connector-node-addresses", []string{}, "internal ip address of all connector nodes")
}

func getRouteTmpl(prefix string) (netlink.Route, error) {
	ip, _, err := net.ParseCIDR(prefix)
	if err != nil {
		return netlink.Route{}, err
	}

	routes, err := netlink.RouteGet(ip)
	if err != nil || len(routes) < 1 {
		return netlink.Route{}, err
	}

	r := netlink.Route{}
	r.Flags = int(netlink.FLAG_ONLINK)
	r.Gw = routes[0].Gw
	r.Dst = routes[0].Dst
	r.LinkIndex = routes[0].LinkIndex

	return r, nil
}

func addRule() error {
	rule := netlink.NewRule()
	rule.Priority = constants.TableStrongswan
	rule.Table = constants.TableStrongswan

	if err := netlink.RuleAdd(rule); err != nil {
		if !routeUtil.FileExistsError(err) {
			return err
		}
	}

	return nil
}

func addAndSaveRoutes(cp routing.ConnectorPrefixes) error {
	// ensure iptables
	go func() {
		klog.V(5).Infof("try to sync ipset")
		if err := syncRemotePodCIDRSet(cp); err != nil {
			klog.Errorf("failed to sync ipset:%s", err)
		}

		klog.V(5).Infof("try to sync iptables forward chain")
		if err := syncForwardRules(); err != nil {
			klog.Errorf("failed to sync iptables forward chain:%s", err)
		}

		klog.V(5).Infof("try to sync iptables post-routing chain")
		if err := syncPostRoutingRules(); err != nil {
			klog.Errorf("failed to sync iptables post-routing chain:%s", err)
		}
	}()

	klog.V(5).Infof("try to sync ip rule")
	if err := addRule(); err != nil {
		return err
	}

	// get the route to connector's local prefix and save it as a template
	rt, err := getRouteTmpl(cp.LocalPrefixes[0])
	if err != nil {
		return err
	}

	klog.V(5).Infof("try to sync routes")
	var routes []netlink.Route
	for _, p := range cp.RemotePrefixes {
		_, prefix, err := net.ParseCIDR(p)
		if err != nil {
			return err
		}
		rt.Dst = prefix
		rt.Table = constants.TableStrongswan

		if err = netlink.RouteReplace(&rt); err != nil {
			klog.Errorf("failed to replace route:%s", err)
		}

		// save the route, for the sake to remove it once the node left
		routes = append(routes, rt)
	}

	addedRoutes[cp.Name] = routes

	return nil
}

func msgHandler(b []byte) {
	debounced(func() {
		var cp routing.ConnectorPrefixes
		if err := json.Unmarshal(b, &cp); err != nil {
			klog.Errorf("failed to unmarshal message:%s", err)
		}
		klog.V(5).Infof("get connector message:%+v", cp)

		if err := addAndSaveRoutes(cp); err != nil {
			klog.Errorf("failed to add route:%s", err)
		}
	})
}

func delAllSavedRoutesByNode(name string) {
	if _, ok := addedRoutes[name]; ok {
		for _, r := range addedRoutes[name] {
			klog.V(5).Infof("delete route: %+v", r)
			if err := netlink.RouteDel(&r); err != nil {
				if !routeUtil.NoSuchProcessError(err) {
					klog.Errorf("failed to delete route:%+v with error:%s", r, err)
				}
			}
		}
		delete(addedRoutes, name)
	}
}

func nodeLeaveHandler(name string) {
	debounced(func() {
		klog.V(5).Infof("node %s leave, to delete all routes via it", name)
		delAllSavedRoutesByNode(name)
	})
}

func Execute() {
	flag.Parse()

	about.DisplayVersion()

	if len(initMembers) < 1 {
		klog.Exit("at least one connector node address is needed")
	}
	mc, err := memberlist.New(initMembers, msgHandler, nodeLeaveHandler)
	if err != nil {
		klog.Exit(err)
	}

	for {
		if len(mc.ListMembers()) < 2 {
			klog.Exit("lost connection to connectors, exit")
		}

		for _, member := range mc.ListMembers() {
			klog.V(5).Infof("Member: %s %s\n", member.Name, member.Addr)
		}

		time.Sleep(time.Minute * 5)
	}
}
