package main

import (
	"database/sql"
	"net/rpc"
	"runtime"
	"strconv"
	"strings"
	"sync"

	log "github.com/avabot/ava/Godeps/_workspace/src/github.com/Sirupsen/logrus"
	"github.com/avabot/ava/shared/datatypes"
	"github.com/avabot/ava/shared/pkg"
)

type Ava int

type pkgMap struct {
	pkgs  map[string]*pkg.PkgWrapper
	mutex *sync.Mutex
}

var regPkgs = pkgMap{
	pkgs:  make(map[string]*pkg.PkgWrapper),
	mutex: &sync.Mutex{},
}

var appVocab dt.AtomicMap

var client *rpc.Client

// RegisterPackage enables Ava to notify packages when specific StructuredInput
// is encountered. Note that packages will only listen when ALL criteria are met
func (t *Ava) RegisterPackage(p *pkg.Pkg, reply *string) error {
	pt := p.Config.Port + 1
	log.WithFields(log.Fields{
		"pkg":  p.Config.Name,
		"port": pt,
	}).Debugln("registering")
	port := ":" + strconv.Itoa(pt)
	addr := p.Config.ServerAddress + port
	cl, err := rpc.Dial("tcp", addr)
	if err != nil {
		return err
	}
	for _, c := range p.Trigger.Commands {
		appVocab.Set(c, true)
		for _, o := range p.Trigger.Objects {
			appVocab.Set(o, true)
			s := strings.ToLower(c + "_" + o)
			if regPkgs.Get(s) != nil {
				log.WithFields(log.Fields{
					"pkg":   p.Config.Name,
					"route": s,
				}).Warnln("duplicate package or trigger")
			}
			regPkgs.Set(s, &pkg.PkgWrapper{P: p, RPCClient: cl})
		}
	}
	if p.Vocab != nil {
		for k := range p.Vocab.Commands {
			appVocab.Set(k, true)
		}
		for k := range p.Vocab.Objects {
			appVocab.Set(k, true)
		}
	}
	return nil
}

func getPkg(m *dt.Msg) (*pkg.PkgWrapper, string, bool, error) {
	var p *pkg.PkgWrapper
	if m.User == nil {
		p = regPkgs.Get("onboard_onboard")
		if p != nil {
			return p, "onboard_onboard", false, nil
		} else {
			log.Errorln("missing required onboard package")
			return p, "onboard_onboard", false, ErrMissingPackage
		}
	}
	si := m.StructuredInput
	var route string
Loop:
	for _, c := range si.Commands {
		for _, o := range si.Objects {
			route = strings.ToLower(c + "_" + o)
			log.Debugln("searching route", route)
			p = regPkgs.Get(route)
			if p != nil {
				break Loop
			}
		}
	}
	if p != nil {
		return p, route, false, nil
	}
	if len(route) == 0 {
		var err error
		log.Debugln("getting last route")
		route, err = m.GetLastRoute(db)
		if err == sql.ErrNoRows {
			log.Errorln("no rows for last response")
		} else if err != nil {
			return p, "", false, err
		}
		log.Debugln("got last route, ", route)
	}
	if len(route) == 0 {
		log.Warnln("no last response")
		return p, route, false, nil
	}
	p = regPkgs.Get(route)
	if p == nil {
		log.Debugln("route", route)
		return p, route, false, ErrMissingPackage
	}
	return p, route, true, nil
}

func callPkg(pw *pkg.PkgWrapper, m *dt.Msg, followup bool) (*dt.RespMsg,
	error) {
	reply := &dt.RespMsg{}
	if pw == nil {
		return reply, nil
	}
	log.WithField("pkg", pw.P.Config.Name).Infoln("sending input")
	c := strings.Title(pw.P.Config.Name)
	// TODO is this OR condition really necessary?
	if followup {
		log.WithField("pkg", pw.P.Config.Name).Infoln("follow up")
		c += ".FollowUp"
	} else {
		log.WithField("pkg", pw.P.Config.Name).Infoln("first run")
		c += ".Run"
	}
	if err := pw.RPCClient.Call(c, m, reply); err != nil {
		log.WithField("pkg", pw.P.Config.Name).Errorln(
			"invalid response", err)
		return reply, err
	}
	return reply, nil
}

func (pm pkgMap) Get(k string) *pkg.PkgWrapper {
	var pw *pkg.PkgWrapper
	pm.mutex.Lock()
	pw = pm.pkgs[k]
	pm.mutex.Unlock()
	runtime.Gosched()
	return pw
}

func (pm pkgMap) Set(k string, v *pkg.PkgWrapper) {
	pm.mutex.Lock()
	pm.pkgs[k] = v
	pm.mutex.Unlock()
	runtime.Gosched()
}
