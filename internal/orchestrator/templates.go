package orchestrator

import (
	"bytes"
	"fmt"
	"text/template"

	"github.com/dihmeetree/harbor/pkg/models"
)

// ControlPlaneTemplate is the docker-compose template for the control plane server
const ControlPlaneTemplate = `version: '3.8'

services:
  # APISIX Control Plane
  apisix-control-plane:
    container_name: apisix-control-plane
    image: apache/apisix:3.13.0-debian
    restart: always
    depends_on:
      - etcd
    ports:
      - '0.0.0.0:9092:9092/tcp'
      - '0.0.0.0:9180:9180/tcp'
    environment:
      - APISIX_STAND_ALONE=false
    volumes:
      - ./apisix-control.yaml:/usr/local/apisix/conf/config.yaml:ro
      - ./apisix/plugins:/opt/apisix/plugins:ro
    networks:
      - apisix

  # etcd
  etcd:
    container_name: etcd
    image: bitnamilegacy/etcd:3.6.4
    restart: always
    environment:
      - ETCD_ENABLE_V2=true
      - ALLOW_NONE_AUTHENTICATION=yes
      - ETCD_ADVERTISE_CLIENT_URLS=http://0.0.0.0:2379
      - ETCD_LISTEN_CLIENT_URLS=http://0.0.0.0:2379
    ports:
      - '0.0.0.0:2379:2379/tcp'
    volumes:
      - etcd_data:/bitnami/etcd
    networks:
      - apisix

  # Prometheus
  prometheus:
    container_name: prometheus
    image: prom/prometheus:v3.5.0
    restart: always
    command:
      - '--web.enable-remote-write-receiver'
      - '--web.enable-lifecycle'
      - '--config.file=/etc/prometheus/prometheus.yml'
      - '--storage.tsdb.path=/prometheus'
    ports:
      - '0.0.0.0:{{.PrometheusPort}}:9090'
    volumes:
      - ./prometheus.yml:/etc/prometheus/prometheus.yml
      - prometheus_data:/prometheus
    networks:
      - apisix

  # cAdvisor
  cadvisor:
    container_name: cadvisor
    image: gcr.io/cadvisor/cadvisor:latest
    restart: always
    ports:
      - '0.0.0.0:{{.CAdvisorPort}}:8080'
    volumes:
      - /:/rootfs:ro
      - /var/run:/var/run:rw
      - /sys:/sys:ro
      - /var/lib/docker/:/var/lib/docker:ro
    networks:
      - apisix

  # Node Exporter
  node-exporter:
    container_name: node-exporter
    image: prom/node-exporter:v1.9.1
    restart: always
    ports:
      - '0.0.0.0:{{.NodeExporterPort}}:9100'
    pid: host
    networks:
      - apisix

  # Grafana
  grafana:
    container_name: grafana
    image: grafana/grafana:12.2.0
    restart: always
    ports:
      - '3000:3000'
    volumes:
      - ./grafana/provisioning:/etc/grafana/provisioning
      - ./grafana/dashboards:/var/lib/grafana/dashboards
      - ./grafana/config/grafana.ini:/etc/grafana/grafana.ini
    networks:
      - apisix
    depends_on:
      - prometheus
{{if .AutoscalerEnabled}}
  # Autoscaler
  autoscaler:
    container_name: autoscaler
    image: itsdmitryhere/harbor-autoscaler:latest
    restart: always
    environment:
      - CONFIG_PATH=/etc/harbor/config.yaml
      - PROMETHEUS_URL=http://prometheus:9090
      - APISIX_URL=http://apisix-control-plane:9180
      - HETZNER_API_TOKEN={{.HetznerToken}}
      - SSH_KEY_PATH=/root/.ssh/id_rsa
    volumes:
      - ./config.yaml:/etc/harbor/config.yaml:ro
      - /root/.ssh:/root/.ssh:ro
      - ./apisix/plugins:/opt/harbor/apisix/plugins:ro
    networks:
      - apisix
    depends_on:
      - prometheus
      - apisix-control-plane
{{end}}
{{if .K6Enabled}}
  # k6 Load Testing
  k6:
    container_name: k6
    image: grafana/k6:latest
    restart: always
    command: run -o experimental-prometheus-rw /scripts/loadtest.js
    environment:
      - K6_PROMETHEUS_RW_SERVER_URL=http://prometheus:9090/api/v1/write
      - K6_PROMETHEUS_RW_TREND_STATS=p(95),p(99),min,max,avg
      - K6_PROMETHEUS_RW_PUSH_INTERVAL=5s
      - LB_TARGETS={{.K6LBTargets}}
      - RATE={{.K6Rate}}
      - DURATION={{.K6Duration}}
      - PREALLOCATED_VUS={{.K6PreallocatedVUs}}
      - MAX_VUS={{.K6MaxVUs}}
      - TARGET_PATH={{.K6TargetPath}}
      - CONNECTION_TIMEOUT={{.K6ConnectionTimeout}}
      - REQUEST_TIMEOUT={{.K6RequestTimeout}}
      - GRACEFUL_STOP={{.K6GracefulStop}}
    volumes:
      - ./k6:/scripts:ro
    networks:
      - apisix
{{end}}

networks:
  apisix:
    driver: bridge

volumes:
  etcd_data:
  prometheus_data:
`

// DataPlaneTemplate is the docker-compose template for data plane servers
const DataPlaneTemplate = `version: '3.8'

services:
  # APISIX Data Plane
  apisix-data-plane:
    container_name: apisix-data-plane
    image: apache/apisix:3.13.0-debian
    restart: always
    ports:
      - '0.0.0.0:80:9080/tcp'
      - '0.0.0.0:443:9443/tcp'
      - '9091:9091/tcp'
    environment:
      - APISIX_STAND_ALONE=false
    volumes:
      - ./apisix-data.yaml:/usr/local/apisix/conf/config.yaml:ro
      - ./apisix/plugins:/opt/apisix/plugins:ro
    networks:
      - apisix

  # cAdvisor
  cadvisor:
    container_name: cadvisor
    image: gcr.io/cadvisor/cadvisor:latest
    restart: always
    ports:
      - '0.0.0.0:{{.CAdvisorPort}}:8080'
    volumes:
      - /:/rootfs:ro
      - /var/run:/var/run:rw
      - /sys:/sys:ro
      - /var/lib/docker/:/var/lib/docker:ro
    networks:
      - apisix

  # Node Exporter
  node-exporter:
    container_name: node-exporter
    image: prom/node-exporter:v1.9.1
    restart: always
    ports:
      - '0.0.0.0:{{.NodeExporterPort}}:9100'
    pid: host
    networks:
      - apisix

networks:
  apisix:
    driver: bridge
`

// AppServerTemplate is the docker-compose template for app servers
const AppServerTemplate = `version: '3.8'

services:
  # Application
  app:
    container_name: app
    image: {{.AppImage}}
    restart: always
    ports:
      - '0.0.0.0:80:80'
    volumes:
      - ./nginx.conf:/etc/nginx/nginx.conf:ro
    networks:
      - apisix

  # cAdvisor
  cadvisor:
    container_name: cadvisor
    image: gcr.io/cadvisor/cadvisor:latest
    restart: always
    ports:
      - '0.0.0.0:{{.CAdvisorPort}}:8080'
    volumes:
      - /:/rootfs:ro
      - /var/run:/var/run:rw
      - /sys:/sys:ro
      - /var/lib/docker/:/var/lib/docker:ro
    networks:
      - apisix

  # Node Exporter
  node-exporter:
    container_name: node-exporter
    image: prom/node-exporter:v1.9.1
    restart: always
    ports:
      - '0.0.0.0:{{.NodeExporterPort}}:9100'
    pid: host
    networks:
      - apisix

networks:
  apisix:
    driver: bridge
`

// APISIXControlPlaneConfigTemplate is the APISIX control plane config
const APISIXControlPlaneConfigTemplate = `apisix:
  enable_admin: true
  enable_control: true
  control:
    ip: '0.0.0.0'
    port: 9092
  extra_lua_path: '/opt/?.lua'

deployment:
  role: control_plane
  role_control_plane:
    config_provider: etcd
  admin:
    allow_admin:
      - 0.0.0.0/0
    admin_key:
      - name: 'admin'
        key: {{.APIKey}}
        role: admin
      - name: 'viewer'
        key: 4054f7cf07e344346cd3f287985e76a2
        role: viewer
    admin_listen:
      ip: '0.0.0.0'
      port: 9180
  etcd:
    host:
      - 'http://etcd:2379'
    prefix: '/apisix'
    timeout: 30

plugins:
  - hello
  - limit-count
  - log-rotate
  - prometheus
  - proxy-cache

plugin_attr:
  log-rotate:
    interval: 21600
    max_kept: 5
    max_size: 26214400
    enable_compression: false
  prometheus:
    export_addr:
      ip: '0.0.0.0'
      port: 9091
  proxy_cache:
    cache_ttl: 10s
`

// APISIXDataPlaneConfigTemplate is the APISIX data plane config
const APISIXDataPlaneConfigTemplate = `apisix:
  enable_http2: true
  enable_ipv6: false
  node_listen: 9080
  ssl:
    enable: true
    ssl_protocols: TLSv1.3
    listen:
      - port: 9443
  extra_lua_path: '/opt/?.lua'

deployment:
  role: data_plane
  role_data_plane:
    config_provider: etcd
  etcd:
    host:
      - 'http://{{.ControlPlaneIP}}:2379'
    prefix: '/apisix'
    timeout: 30

plugins:
  - hello
  - limit-count
  - log-rotate
  - prometheus
  - proxy-cache

plugin_attr:
  log-rotate:
    interval: 21600
    max_kept: 5
    max_size: 26214400
    enable_compression: false
  prometheus:
    export_addr:
      ip: '0.0.0.0'
      port: 9091
  proxy_cache:
    cache_ttl: 10s
`

// PrometheusConfigTemplate is the Prometheus configuration
const PrometheusConfigTemplate = `global:
  scrape_interval: 5s
  external_labels:
    stack: 'apisix'

scrape_configs:
  - job_name: prometheus
    scrape_interval: 5s
    static_configs:
      - targets: ['localhost:9090']

  - job_name: apisix
    scrape_interval: 5s
    metrics_path: '/apisix/prometheus/metrics'
    static_configs:
      - targets: ['apisix-control-plane:9091'{{range .DataPlaneIPs}}, '{{.}}:9091'{{end}}]

  - job_name: node-exporter
    scrape_interval: 5s
    static_configs:
      - targets: ['node-exporter:9100']
        labels:
          server: 'harbor-control'
{{range .DataPlanes}}      - targets: ['{{.PrivateIP}}:9100']
        labels:
          server: '{{.Name}}'
{{end}}{{range .AppServers}}      - targets: ['{{.PrivateIP}}:9100']
        labels:
          server: '{{.Name}}'
{{end}}
  - job_name: cadvisor
    scrape_interval: 5s
    static_configs:
      - targets: ['cadvisor:8080']
        labels:
          server: 'harbor-control'
{{range .DataPlanes}}      - targets: ['{{.PrivateIP}}:8080']
        labels:
          server: '{{.Name}}'
{{end}}{{range .AppServers}}      - targets: ['{{.PrivateIP}}:8080']
        labels:
          server: '{{.Name}}'
{{end}}
`

// NginxConfigTemplate is the nginx configuration for app servers
const NginxConfigTemplate = `worker_processes 1;
error_log stderr notice;
events {
    worker_connections 1024;
}

http {
    variables_hash_max_size 1024;
    access_log off;
    real_ip_header X-Real-IP;
    charset utf-8;

    server {
        listen 80;

        location / {
            add_header Cache-Control "no-store, no-cache, must-revalidate, proxy-revalidate, max-age=0";
            add_header Pragma "no-cache";
            add_header Expires "0";
            return 200 "hello {{.ServerID}}";
        }
    }
}
`

// TemplateData holds data for template rendering
type TemplateData struct {
	PrometheusPort      int
	CAdvisorPort        int
	NodeExporterPort    int
	AppImage            string
	APIKey              string
	ControlPlaneIP      string
	DataPlaneIPs        []string
	AppServerIPs        []string
	DataPlanes          []*models.Server
	AppServers          []*models.Server
	ServerID            string
	AutoscalerEnabled   bool
	HetznerToken        string
	K6Enabled           bool
	K6PreallocatedVUs   int
	K6MaxVUs            int
	K6Rate              int
	K6Duration          string
	K6TargetPath        string
	K6ConnectionTimeout string
	K6RequestTimeout    string
	K6GracefulStop      string
	K6LBTargets         string
}

// RenderTemplate renders a template with the given data
func RenderTemplate(tmpl string, data interface{}) (string, error) {
	t, err := template.New("config").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("failed to parse template: %w", err)
	}

	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("failed to execute template: %w", err)
	}

	return buf.String(), nil
}
