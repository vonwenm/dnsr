package dnsr

import (
	"time"

	"github.com/miekg/dns"
)

var (
	Timeout        = 2000 * time.Millisecond
	MaxRecursion   = 10
	MaxNameservers = 2
	MaxIPs         = 2
)

// Resolver implements a primitive, non-recursive, caching DNS resolver.
type Resolver struct {
	cache  *cache
	client *dns.Client
}

// New initializes a Resolver with the specified cache size.
func New(capacity int) *Resolver {
	r := &Resolver{
		cache: newCache(capacity),
		client: &dns.Client{
			DialTimeout:  Timeout,
			ReadTimeout:  Timeout,
			WriteTimeout: Timeout,
		},
	}
	return r
}

// Resolve finds DNS records of type qtype for the domain qname. It returns a slice of *RR.
// For nonexistent domains (where a DNS server will return NXDOMAIN), it will return an empty, non-nil slice.
// Specify an empty string in qtype to receive any DNS records found (currently A, AAAA, NS, CNAME, and TXT).
func (r *Resolver) Resolve(qname string, qtype string) []*RR {
	return r.resolve(qname, qtype, 0)
}

func (r *Resolver) resolve(qname string, qtype string, depth int) []*RR {
	if depth++; depth > MaxRecursion {
		logMaxRecursion(qname, qtype, depth)
		return nil
	}
	qname = toLowerFQDN(qname)
	rrs := r.cacheGet(qname, qtype)
	if rrs != nil {
		return rrs
	}
	logResolveStart(qname, qtype, depth)
	rrs = r.iterateParents(qname, qtype, depth)
	logResolveEnd(qname, qtype, rrs, depth, time.Now())
	return rrs
}

func (r *Resolver) iterateParents(qname string, qtype string, depth int) []*RR {
	success := make(chan bool, 1)
	for pname, ok := qname, true; ok; pname, ok = parent(pname) {
		if pname == qname && qtype == "NS" { // If we’re looking for [foo.com,NS], then skip to [com,NS]
			continue
		}

		// Only query TLDs against the root nameservers
		if pname == "." && dns.CountLabel(qname) >= 2 {
			// fmt.Fprintf(os.Stderr, "Warning: non-TLD query at root: dig +norecurse %s %s\n", qname, qtype)
			return nil
		}

		// Get nameservers
		nrrs := r.resolve(pname, "NS", depth)

		// Short circuit on error (e.g. MaxRecursion)
		if nrrs == nil {
			return nil // FIXME: use an error instead of nil
		}

		// Query all nameservers in parallel
		count := 0
		for _, nrr := range nrrs {
			if qtype != "" { // Early out for specific queries
				if rrs := r.cacheGet(qname, qtype); rrs != nil {
					return rrs
				}
			}
			if nrr.Type != "NS" {
				continue
			}
			if count++; count > MaxNameservers {
				break
			}

			go r.exchange(success, nrr.Value, qname, qtype, depth)
		}

		// Wait for first response
		if count > 0 {
			select {
			case <-success:
				return r.resolveCNAMEs(qname, qtype, depth)
			case <-time.After(Timeout):
				continue
			}
		}

		// NS queries naturally recurse, so stop further iteration
		if qtype == "NS" {
			return []*RR{}
		}
	}
	return nil
}

func (r *Resolver) exchange(success chan<- bool, host string, qname string, qtype string, depth int) {
	dtype := dns.StringToType[qtype]
	if dtype == 0 {
		dtype = dns.TypeA
	}
	qmsg := &dns.Msg{}
	qmsg.SetQuestion(qname, dtype)
	qmsg.MsgHdr.RecursionDesired = false

	// Find each A record for the DNS server
	count := 0
	for _, rr := range r.resolve(host, "A", depth) {
		if rr.Type != "A" { // FIXME: support AAAA records?
			continue
		}

		// Never query more than MaxIPs for any nameserver
		if count++; count > MaxIPs {
			return
		}

		// Synchronously query this DNS server
		start := time.Now()
		rmsg, _, err := r.client.Exchange(qmsg, rr.Value+":53")
		logExchange(host, qmsg, depth, start, err) // Log hostname instead of IP
		if err != nil {
			continue
		}

		// FIXME: cache NXDOMAIN responses responsibly
		if rmsg.Rcode == dns.RcodeNameError {
			r.cache.add(qname, nil)
		}

		// Cache records returned
		r.saveDNSRR(host, qname, append(append(rmsg.Answer, rmsg.Ns...), rmsg.Extra...)...)

		// Never block
		select {
		case success <- true:
		default:
		}

		// Return after first successful network request
		return
	}
}

func (r *Resolver) resolveCNAMEs(qname string, qtype string, depth int) []*RR {
	rrs := []*RR{} // Return non-nil slice indicating difference between NXDOMAIN and an error
	for _, crr := range r.cacheGet(qname, "") {
		rrs = append(rrs, crr)
		if crr.Type != "CNAME" {
			continue
		}
		logCNAME(crr.String(), depth)
		for _, rr := range r.resolve(crr.Value, qtype, depth) {
			r.cache.add(qname, rr)
			rrs = append(rrs, crr)
		}
	}
	return rrs
}

// saveDNSRR saves 1 or more DNS records to the resolver cache.
func (r *Resolver) saveDNSRR(host string, qname string, drrs ...dns.RR) {
	cl := dns.CountLabel(qname)
	for _, drr := range drrs {
		if rr := convertRR(drr); rr != nil {
			if dns.CountLabel(rr.Name) < cl && dns.CompareDomainName(qname, rr.Name) < 2 {
				// fmt.Fprintf(os.Stderr, "Warning: potential poisoning from %s: %s -> %s\n", host, qname, drr.String())
				continue
			}
			r.cache.add(rr.Name, rr)
		}
	}
}

// cacheGet returns a randomly ordered slice of DNS records.
func (r *Resolver) cacheGet(qname string, qtype string) []*RR {
	any := r.cache.get(qname)
	if any == nil {
		any = rootCache.get(qname)
	}
	if any == nil || len(any) == 0 {
		return any
	}
	rrs := make([]*RR, 0, len(any))
	for _, rr := range any {
		if qtype == "" || rr.Type == qtype {
			rrs = append(rrs, rr)
		}
	}
	if len(rrs) == 0 && (qtype != "" && qtype != "NS") {
		return nil
	}
	return rrs
}
