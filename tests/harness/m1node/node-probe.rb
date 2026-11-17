# frozen_string_literal: true

# The harness's probe of module 1's verifier node, run INSIDE the node container (stage M1N-2): `docker compose exec -T verifier-node ruby - <command> [args] < node-probe.rb`.
# It uses only the standard library (plain `ruby`, no bundler), reads the node's own token and resolver from the container's environment (the token is never on a command line
# and never printed), and prints facts as `STEP ...`, `AUDIT ...`, `SUMMARY ...` and raw lines; tests/harness/m1node/m1node.sh decides what is red.
#
#   steps            the three things the gem's host audit needs, one by one, the way the gem does them (truemail 3.3.1 audit/ip.rb, dns.rb, ptr.rb): the HTTPS "what is my IP" answer
#                    through the OS resolver and the OS trust store (SSL_CERT_FILE), then the A record of VERIFIER_DOMAIN and the PTR of that address through the resolver the gem
#                    uses (the DNS variable). Each step prints ok=true|false so the first one to fail names itself.
#   hygiene          resolv.conf and hostname as this process sees them
#   audit            GET /v1/audit with the node token: the raw answer (audit_status, warnings, current_host_ip)
#   verify <email>   POST /v1/verify exactly as module 1's control plane does ({"email": ...} only): the raw answer and a one-line SUMMARY of what the SMTP fixture made of it

require 'json'
require 'net/http'
require 'resolv'
require 'ipaddr'
require 'socket'

TOKEN = ENV.fetch('ACCESS_TOKENS').split(',').first.strip
DNS_SERVER = ENV.fetch('DNS').split(',').first.strip
VERIFIER_DOMAIN = ENV.fetch('VERIFIER_DOMAIN')

def node_call(path, body = nil)
  uri = URI("http://127.0.0.1:3000#{path}")
  req = body ? Net::HTTP::Post.new(uri) : Net::HTTP::Get.new(uri)
  req['Authorization'] = TOKEN # the raw token, not Bearer (the node's own convention)
  if body
    req['Content-Type'] = 'application/json'
    req.body = JSON.generate(body)
  end
  Net::HTTP.start(uri.host, uri.port, read_timeout: 90) { |http| http.request(req) }
end

def steps
  ip = nil
  begin
    ip = Net::HTTP.get(URI('https://api.ipify.org'))
    puts "STEP ipify ok=#{!ip.empty? && ip !~ /\s/} body=#{ip.inspect} bytes=#{ip.bytesize}"
  rescue StandardError => e
    puts "STEP ipify ok=false error=#{e.class}: #{e.message}"
  end
  resolver = Resolv::DNS.new(nameserver_port: [[DNS_SERVER, 53]])
  begin
    a = resolver.getaddress(VERIFIER_DOMAIN).to_s
    puts "STEP a_record ok=#{a == ip} name=#{VERIFIER_DOMAIN} value=#{a.inspect} equals_ipify=#{a == ip}"
  rescue StandardError => e
    puts "STEP a_record ok=false name=#{VERIFIER_DOMAIN} error=#{e.class}: #{e.message}"
  end
  if ip && !ip.empty?
    begin
      reverse = IPAddr.new(ip).reverse
      names = resolver.getresources(reverse, Resolv::DNS::Resource::IN::PTR).map { |r| r.name.to_s }
      puts "STEP ptr ok=#{names.include?(VERIFIER_DOMAIN)} reverse=#{reverse} names=#{names.inspect} includes_verifier_domain=#{names.include?(VERIFIER_DOMAIN)}"
    rescue StandardError => e
      puts "STEP ptr ok=false error=#{e.class}: #{e.message}"
    end
  else
    puts 'STEP ptr ok=false error=no address from the first step'
  end
end

def hygiene
  puts "HYGIENE hostname=#{Socket.gethostname.inspect}"
  puts "HYGIENE resolv.conf:"
  File.read('/etc/resolv.conf').each_line { |l| puts "HYGIENE   | #{l.chomp}" }
  puts "HYGIENE SSL_CERT_FILE=#{ENV['SSL_CERT_FILE'].inspect} DNS=#{ENV['DNS'].inspect} SMTP_PORT=#{ENV['SMTP_PORT'].inspect} PROBE_ALLOW_NON_PUBLIC=#{ENV['PROBE_ALLOW_NON_PUBLIC'].inspect}"
end

def audit
  res = node_call('/v1/audit')
  puts "AUDIT http=#{res.code}"
  puts res.body
end

# The one-line view of the node's answer: what the gem saw at RCPT TO for the target, and what the catch-all probe concluded.
def summarize(json)
  j = JSON.parse(json)
  hosts = j['smtp_debug'] || []
  first = hosts.first || {}
  errors = first['errors'] || {}
  rcpt =
    if j['probe_refused']
      'not-attempted'
    elsif hosts.empty?
      j['success'] ? '250(clean)' : 'none'
    elsif first['rcptto'].is_a?(Hash)
      first['rcptto']['status'].to_s
    else
      errors['rcptto'].to_s.strip[0, 70]
    end
  cat = j['catchall'] || {}
  format('success=%<s>s validation_type=%<v>s probe_refused=%<p>s port_opened=%<o>s rcpt=%<r>s catchall=%<c>s mx=%<m>s',
         s: j['success'], v: j['validation_type'], p: !j['probe_refused'].nil?, o: first['port_opened'].inspect, r: rcpt.inspect,
         c: cat['conclusion'] || 'none', m: (j['mx'] || {})['mail_servers'].inspect)
end

def verify(email)
  t0 = Process.clock_gettime(Process::CLOCK_MONOTONIC)
  res = node_call('/v1/verify', { 'email' => email })
  secs = (Process.clock_gettime(Process::CLOCK_MONOTONIC) - t0).round(1)
  puts "VERIFY http=#{res.code} seconds=#{secs}"
  if res.code == '200'
    puts "SUMMARY #{summarize(res.body)}"
  else
    puts 'SUMMARY http-error'
  end
  puts "RAW #{res.body}"
end

case ARGV.shift
when 'steps' then steps
when 'hygiene' then hygiene
when 'audit' then audit
when 'verify' then verify(ARGV.fetch(0))
else
  warn 'usage: ruby - steps|hygiene|audit|verify <email>'
  exit 2
end
