package redis

import (
	"fmt"
	"strings"
	"time"

	"context"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/request"
	"github.com/miekg/dns"
)

// ServeDNS implements the plugin.Handler interface.
func (redis *Redis) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	state := request.Request{W: w, Req: r}

	qname := state.Name()
	qtype := state.Type()

	if time.Since(redis.LastZoneUpdate) > redis.zoneUpdateTime {
		redis.LoadZones()
	}

	zone := plugin.Zones(redis.Zones).Matches(qname)

	if zone == "" {
		return plugin.NextOrFailure(qname, redis.Next, ctx, w, r)
	}

	z := redis.load(zone)
	if z == nil {
		return redis.errorResponse(state, zone, dns.RcodeServerFailure, nil)
	}

	if qtype == "AXFR" {
		records := redis.AXFR(z)

		ch := make(chan *dns.Envelope)
		tr := new(dns.Transfer)
		tr.TsigSecret = nil

		go func(ch chan *dns.Envelope) {
			j, l := 0, 0

			for i, r := range records {
				l += dns.Len(r)
				if l > transferLength {
					ch <- &dns.Envelope{RR: records[j:i]}
					l = 0
					j = i
				}
			}
			if j < len(records) {
				ch <- &dns.Envelope{RR: records[j:]}
			}
			close(ch)
		}(ch)

		err := tr.Out(w, r, ch)
		if err != nil {
			fmt.Println(err)
		}
		w.Hijack()
		return dns.RcodeSuccess, nil
	}

	location := redis.findLocation(qname, z)
	if len(location) == 0 { // empty, no results
		return redis.errorResponse(state, zone, dns.RcodeNameError, nil)
	}

	answers := make([]dns.RR, 0, 10)
	extras := make([]dns.RR, 0, 10)

	record := redis.get(location, z)

	switch qtype {
	case "A":
		// Having a CNAME excludes A records, add cnames when querying for A records
		if len(record.CNAME) > 0 {
			//println("We have a cname in the record")
			answers2 := make([]dns.RR, 0, 10)
			extras2 := make([]dns.RR, 0, 10)
			answers2, extras2 = redis.CNAME(qname, z, record)
			answers = append(answers, answers2...)
			extras = append(extras, extras2...)
		} else {
			answers, extras = redis.A(qname, z, record)
		}
	case "AAAA":
		answers, extras = redis.AAAA(qname, z, record)
	case "CNAME":
		answers, extras = redis.CNAME(qname, z, record)
	case "TXT":
		answers, extras = redis.TXT(qname, z, record)
	case "NS":
		answers, extras = redis.NS(qname, z, record)
	case "MX":
		answers, extras = redis.MX(qname, z, record)
	case "SRV":
		answers, extras = redis.SRV(qname, z, record)
	case "SOA":
		answers, extras = redis.SOA(qname, z, record)
	case "CAA":
		answers, extras = redis.CAA(qname, z, record)

	default:
		return redis.errorResponse(state, zone, dns.RcodeNotImplemented, nil)
	}

	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative, m.RecursionAvailable, m.Compress = true, false, true

	m.Answer = append(m.Answer, answers...)
	m.Extra = append(m.Extra, extras...)

	if len(record.CNAME) > 0 {
		// If there is a CNAME RR in the answers, solve the alias
		for _, CNAMERecord := range record.CNAME {
			println(CNAMERecord.Host)
			var query = strings.TrimSuffix(CNAMERecord.Host, "."+z.Name)
			records := redis.get(query, z)

			answersN := make([]dns.RR, 0, 10)
			extrasN := make([]dns.RR, 0, 10)
			answersN, extrasN = redis.A(CNAMERecord.Host, z, records)
			m.Answer = append(m.Answer, answersN...)
			m.Extra = append(m.Extra, extrasN...)

		}
	}

	state.SizeAndDo(m)
	m = state.Scrub(m)
	_ = w.WriteMsg(m)
	return dns.RcodeSuccess, nil
}

// Name implements the Handler interface.
func (redis *Redis) Name() string { return "redis" }

func (redis *Redis) errorResponse(state request.Request, zone string, rcode int, err error) (int, error) {
	m := new(dns.Msg)
	m.SetRcode(state.Req, rcode)
	m.Authoritative, m.RecursionAvailable, m.Compress = true, false, true

	state.SizeAndDo(m)
	_ = state.W.WriteMsg(m)
	// Return success as the rcode to signal we have written to the client.
	return dns.RcodeSuccess, err
}
