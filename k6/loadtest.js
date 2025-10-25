import http from "k6/http";
import { check, sleep } from "k6";
import { Rate } from "k6/metrics";

// Custom metrics
const errorRate = new Rate("errors");

// Load balancer targets from environment
const targets = __ENV.LB_TARGETS
  ? __ENV.LB_TARGETS.split(",")
  : ["http://localhost"];
const targetPath = __ENV.TARGET_PATH || "/";

// Round-robin counter (shared across VUs)
let currentTargetIndex = 0;

// Configuration from environment variables
export const options = {
  scenarios: {
    constant_rate: {
      executor: "constant-arrival-rate",
      rate: parseInt(__ENV.RATE) || 10, // Requests per second
      timeUnit: "1s",
      duration: __ENV.DURATION || "30s",
      preAllocatedVUs: parseInt(__ENV.PREALLOCATED_VUS) || 10,
      maxVUs: parseInt(__ENV.MAX_VUS) || 100,
    },
  },
  thresholds: {
    http_req_duration: ["p(95)<500"], // 95% of requests should be below 500ms
    errors: ["rate<0.1"], // Error rate should be below 10%
  },
  // Connection and timeout settings
  noConnectionReuse: false,
  userAgent: "k6-harbor-loadtest/1.0",
};

// Main test function
export default function () {
  // Round-robin load balancer selection
  const target = targets[currentTargetIndex % targets.length];
  currentTargetIndex++;
  const url = `${target}${targetPath}`;

  const params = {
    headers: {
      "Content-Type": "application/json",
    },
    timeout: __ENV.REQUEST_TIMEOUT || "30s",
  };

  const response = http.get(url, params);

  // Check response
  const result = check(response, {
    "status is 200": (r) => r.status === 200,
    "response time < 500ms": (r) => r.timings.duration < 500,
  });

  // Track errors
  errorRate.add(!result);

  // Optional: Add think time between requests
  // sleep(0.1);
}

// Lifecycle hooks
export function setup() {
  console.log("Starting load test");
  console.log(`Targets: ${targets.join(", ")}`);
  console.log(`Path: ${targetPath}`);
  console.log(`Rate: ${__ENV.RATE || 10} req/s`);
  console.log(`Duration: ${__ENV.DURATION || "30s"}`);
  console.log(`Preallocated VUs: ${__ENV.PREALLOCATED_VUS || 10}`);
  console.log(`Max VUs: ${__ENV.MAX_VUS || 100}`);
}

export function teardown(data) {
  console.log("Load test completed");
}
