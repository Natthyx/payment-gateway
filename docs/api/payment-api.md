# Payment Gateway — API Reference

## Base URL

```
http://localhost:8080  (development)
https://api.yourdomain.com  (production)
```

---

## Authentication

Most endpoints require API key authentication via Bearer token:

```
Authorization: Bearer gw_live_<lookup>_<secret>
```

**API Key Format:**
- Environment: `live` or `test`
- Lookup: 16 hex characters (indexed)
- Secret: 48 hex characters (bcrypt-hashed)

**Example:**
```
Authorization: Bearer gw_live_a1b2c3d4e5f6g7h8_0123456789abcdef0123456789abcdef0123456789abcdef
```

---

## Response Format

All API responses use a consistent envelope:

### Success Response
```json
{
  "success": true,
  "data": { ... },
  "meta": {
    "total": 142,
    "page": 1,
    "limit": 20
  }
}
```

### Error Response
```json
{
  "success": false,
  "error_obj": {
    "code": "idempotency_conflict",
    "message": "idempotency_key already used"
  }
}
```

### Standard Error Codes
- `invalid_request` — Validation error
- `unauthorized` — Missing or invalid API key
- `forbidden` — Valid key but insufficient permissions
- `not_found` — Resource not found
- `conflict` — Resource already exists
- `idempotency_conflict` — Idempotency key already used with different parameters
- `rate_limit_exceeded` — Too many requests
- `provider_error` — Payment provider error
- `internal_error` — Server error

---

## Endpoints

### 1. Health Check

Check service status.

```http
GET /health
```

**Authentication:** None required

**Response:**
```json
{
  "status": "ok",
  "service": "payment-gateway"
}
```

---

### 2. Prometheus Metrics

```http
GET /metrics
```

**Authentication:** None required

**Response:** Prometheus exposition format

**Metrics Include:**
- `gateway_http_requests_total`
- `gateway_http_request_duration_seconds`
- `gateway_payments_created_total`
- `gateway_webhooks_processed_total`
- `gateway_ledger_entries_written_total`

---

### 3. Create Merchant

Onboard a new merchant and generate API key.

```http
POST /merchants
Content-Type: application/json
```

**Authentication:** None required (public endpoint)

**Request Body:**
```json
{
  "name": "Acme Corp",
  "email": "ops@acme.com"
}
```

**Response:** `201 Created`
```json
{
  "success": true,
  "data": {
    "merchant": {
      "id": 42,
      "name": "Acme Corp",
      "email": "ops@acme.com",
      "is_active": true,
      "created_at": "2024-01-15T10:30:45Z",
      "updated_at": "2024-01-15T10:30:45Z"
    },
    "api_key": "gw_live_a1b2c3d4e5f6g7h8_0123456789abcdef0123456789abcdef0123456789abcdef"
  }
}
```

**⚠️ Important:** The `api_key` is returned ONCE and cannot be retrieved again. Store it securely.

**Error Responses:**
- `400 Bad Request` — Missing name or email
- `409 Conflict` — Email already registered

---

### 4. Create Payment Intent

Initiate a payment.

```http
POST /payments
Authorization: Bearer <api_key>
Idempotency-Key: <unique_key>
Content-Type: application/json
```

**Authentication:** Required

**Headers:**
- `Authorization: Bearer <api_key>` — Required
- `Idempotency-Key: <string>` — Required (prevents duplicate charges)

**Request Body:**
```json
{
  "amount": 1050,
  "currency": "USD",
  "provider": "stripe",
  "metadata": {
    "order_id": "order_12345",
    "customer_email": "customer@example.com"
  }
}
```

**Field Descriptions:**
- `amount` (integer, required): Amount in smallest currency unit (cents/santim). Must be > 0.
- `currency` (string, required): ISO currency code (e.g., "USD", "ETB")
- `provider` (string, required): Payment provider. One of: `"stripe"`, `"chapa"`, `"telebirr"`
- `metadata` (object, optional): Arbitrary key-value data (stored as JSONB)

**Response:** `201 Created`
```json
{
  "success": true,
  "data": {
    "payment_intent": {
      "id": 123,
      "merchant_id": 42,
      "idempotency_key": "order_12345",
      "amount": 1050,
      "currency": "USD",
      "provider": "stripe",
      "status": "processing",
      "provider_tx_id": "pi_abc123",
      "metadata": {
        "order_id": "order_12345",
        "customer_email": "customer@example.com"
      },
      "created_at": "2024-01-15T10:30:45Z",
      "updated_at": "2024-01-15T10:30:45Z"
    },
    "checkout_url": "https://checkout.stripe.com/pay/cs_test_abc123"
  }
}
```

**Payment Statuses:**
- `created` — Intent created, awaiting provider call
- `processing` — Provider charge initiated, awaiting payment
- `succeeded` — Payment successful, funds captured
- `failed` — Payment failed
- `refunded` — Payment refunded

**Next Steps:**
1. Redirect customer to `checkout_url`
2. Customer completes payment on provider's page
3. Provider sends webhook to gateway
4. Gateway updates payment status to `succeeded`
5. Gateway sends webhook to your registered endpoint

**Error Responses:**
- `400 Bad Request` — Invalid amount, currency, or provider
- `401 Unauthorized` — Invalid API key
- `409 Conflict` — Idempotency key already used
- `429 Too Many Requests` — Rate limit exceeded
- `500 Internal Server Error` — Provider error

**Idempotency Behavior:**
- First request: Creates payment, returns `201 Created`
- Duplicate request (same key): Returns cached response, no provider call
- Duplicate with different params: Returns `409 Conflict`

---

### 5. Get Payment Intent

Retrieve a payment intent by ID.

```http
GET /payments/{id}
Authorization: Bearer <api_key>
```

**Authentication:** Required

**Path Parameters:**
- `id` (integer): Payment intent ID

**Response:** `200 OK`
```json
{
  "success": true,
  "data": {
    "id": 123,
    "merchant_id": 42,
    "idempotency_key": "order_12345",
    "amount": 1050,
    "currency": "USD",
    "provider": "stripe",
    "status": "succeeded",
    "provider_tx_id": "pi_abc123",
    "metadata": {
      "order_id": "order_12345"
    },
    "created_at": "2024-01-15T10:30:45Z",
    "updated_at": "2024-01-15T10:31:20Z"
  }
}
```

**Error Responses:**
- `401 Unauthorized` — Invalid API key
- `404 Not Found` — Payment intent not found or not owned by merchant

---

### 6. List Payment Intents

List all payment intents for the authenticated merchant.

```http
GET /payments?page=1&limit=20
Authorization: Bearer <api_key>
```

**Authentication:** Required

**Query Parameters:**
- `page` (integer, optional): Page number (default: 1)
- `limit` (integer, optional): Results per page (default: 20, max: 100)

**Response:** `200 OK`
```json
{
  "success": true,
  "data": [
    {
      "id": 123,
      "amount": 1050,
      "currency": "USD",
      "provider": "stripe",
      "status": "succeeded",
      "created_at": "2024-01-15T10:30:45Z"
    },
    {
      "id": 122,
      "amount": 2500,
      "currency": "USD",
      "provider": "chapa",
      "status": "processing",
      "created_at": "2024-01-15T09:15:30Z"
    }
  ],
  "meta": {
    "total": 142,
    "page": 1,
    "limit": 20
  }
}
```

---

### 7. Create Refund

Refund a payment (full or partial).

```http
POST /payments/{id}/refunds
Authorization: Bearer <api_key>
Idempotency-Key: <unique_key>
Content-Type: application/json
```

**Authentication:** Required

**Path Parameters:**
- `id` (integer): Payment intent ID

**Headers:**
- `Idempotency-Key: <string>` — Required (prevents duplicate refunds)

**Request Body:**
```json
{
  "amount": 1050,
  "reason": "customer request"
}
```

**Field Descriptions:**
- `amount` (integer, optional): Amount to refund. If omitted, refunds full amount.
- `reason` (string, optional): Refund reason

**Response:** `201 Created`
```json
{
  "success": true,
  "data": {
    "id": 45,
    "payment_intent_id": 123,
    "merchant_id": 42,
    "idempotency_key": "refund_order_12345",
    "amount": 1050,
    "currency": "USD",
    "reason": "customer request",
    "status": "succeeded",
    "provider_refund_id": "re_abc123",
    "created_at": "2024-01-15T11:00:00Z",
    "updated_at": "2024-01-15T11:00:05Z"
  }
}
```

**Refund Statuses:**
- `pending` — Refund initiated with provider
- `succeeded` — Refund successful
- `failed` — Refund failed

**Ledger Entries:**
Refunds create inverse ledger entries:
```
DEBIT  merchant:42:refund:1050    -1050
CREDIT provider:stripe:refund:1050 +1050
```

**Error Responses:**
- `400 Bad Request` — Amount exceeds payment amount or payment not succeeded
- `404 Not Found` — Payment not found
- `409 Conflict` — Idempotency key already used

**Provider Support:**
- ✅ Stripe: Fully supported
- ⚠️ Chapa: Partial support (check provider)
- ❌ Telebirr: Not supported

---

### 8. Register Webhook Endpoint

Register a URL to receive payment events.

```http
POST /webhook-endpoints
Authorization: Bearer <api_key>
Content-Type: application/json
```

**Authentication:** Required

**Request Body:**
```json
{
  "url": "https://merchant.example.com/webhooks/payments"
}
```

**Response:** `201 Created`
```json
{
  "success": true,
  "data": {
    "id": 5,
    "merchant_id": 42,
    "url": "https://merchant.example.com/webhooks/payments",
    "is_active": true,
    "created_at": "2024-01-15T10:30:45Z"
  }
}
```

**Webhook Events Sent:**
- `payment.succeeded` — Payment completed successfully
- `refund.created` — Refund initiated

**Webhook Format:**
```http
POST https://merchant.example.com/webhooks/payments
Content-Type: application/json
X-Gateway-Signature: t=1705319445,v1=abc123...
Idempotency-Key: dlv_123
```

**Webhook Body:**
```json
{
  "event_type": "payment.succeeded",
  "merchant_id": 42,
  "data": {
    "id": 123,
    "amount": 1050,
    "currency": "USD",
    "status": "succeeded",
    "provider_tx_id": "pi_abc123"
  },
  "created_at": "2024-01-15T10:31:20.123456Z"
}
```

**Verifying Webhook Signatures:**

```javascript
const crypto = require('crypto');

function verifyWebhook(body, signature, secret) {
  // Parse signature header: "t=1705319445,v1=abc123..."
  const parts = signature.split(',');
  let timestamp, sig;
  
  parts.forEach(part => {
    const [key, value] = part.split('=');
    if (key === 't') timestamp = value;
    if (key === 'v1') sig = value;
  });
  
  // Check timestamp within 5 minute window (replay protection)
  const now = Math.floor(Date.now() / 1000);
  if (Math.abs(now - parseInt(timestamp)) > 300) {
    throw new Error('Timestamp outside 5-minute window');
  }
  
  // Compute expected HMAC-SHA256
  const payload = timestamp + '.' + body;
  const hmac = crypto.createHmac('sha256', secret);
  hmac.update(payload);
  const expected = hmac.digest('hex');
  
  // Constant-time comparison
  if (!crypto.timingSafeEqual(Buffer.from(expected), Buffer.from(sig))) {
    throw new Error('Signature verification failed');
  }
  
  return true;
}

// Usage
app.post('/webhooks/payments', (req, res) => {
  const signature = req.headers['x-gateway-signature'];
  const secret = process.env.OUTBOUND_WEBHOOK_SIGNING_SECRET;
  
  try {
    verifyWebhook(req.body, signature, secret);
    // Process webhook...
    res.status(200).json({ received: true });
  } catch (err) {
    res.status(401).json({ error: 'Invalid signature' });
  }
});
```

**Retry Behavior:**
- Non-2xx response → Exponential backoff retry
- Schedule: 30s, 1m, 2m, 4m, 8m, 16m, 32m, 64m
- After 8 attempts → Dead letter (manual investigation)

**Error Responses:**
- `400 Bad Request` — Invalid URL
- `409 Conflict` — URL already registered

---

### 9. Incoming Webhooks (Provider → Gateway)

These endpoints receive webhooks from payment providers. **Not called by merchants.**

```http
POST /webhooks/stripe
POST /webhooks/chapa
POST /webhooks/telebirr
```

**Authentication:** Provider signature verification

**Processing:**
1. Store raw webhook in `webhooks` table (inbox pattern)
2. Return `200 OK` immediately (< 50ms)
3. Background worker verifies signature asynchronously
4. Updates payment status + writes ledger entries
5. Enqueues merchant webhook

**Idempotency:**
- Provider re-delivers → silently swallowed at boundary
- `UNIQUE(provider, provider_event_id)` prevents duplicates

**Security:**
- Stripe: HMAC-SHA256 with `Stripe-Signature` header
- Chapa: Custom signature verification
- Telebirr: RSA signature verification

---

## Rate Limits

### Global Rate Limit (Per IP)
- **Limit:** 200 requests per minute
- **Response:** `429 Too Many Requests`
- **Reset:** 1 minute

### Per-Merchant Rate Limit
- **Limit:** 600 requests per minute (configurable)
- **Applies to:** Authenticated endpoints only
- **Response:** `429 Too Many Requests`
- **Reset:** 1 minute

**Headers on 429 Response:**
```http
HTTP/1.1 429 Too Many Requests
Retry-After: 60
```

---

## Testing

### Test Environment

Set `ENV=test` to use test API keys:

```
gw_test_<lookup>_<secret>
```

Test keys are isolated from live data.

### Test Cards (Stripe)

- Success: `4242 4242 4242 4242`
- Decline: `4000 0000 0000 0002`
- Insufficient funds: `4000 0000 0000 9995`

---

## Common Workflows

### Complete Payment Flow

```javascript
// 1. Create payment intent
const response = await fetch('http://localhost:8080/payments', {
  method: 'POST',
  headers: {
    'Authorization': 'Bearer gw_live_...',
    'Idempotency-Key': 'order_12345',
    'Content-Type': 'application/json'
  },
  body: JSON.stringify({
    amount: 1050,
    currency: 'USD',
    provider: 'stripe'
  })
});

const { data } = await response.json();
const { checkout_url, payment_intent } = data;

// 2. Redirect user to checkout_url
window.location.href = checkout_url;

// 3. User completes payment on Stripe

// 4. Stripe sends webhook to gateway

// 5. Gateway sends webhook to your endpoint
// POST https://yourdomain.com/webhooks/payments
// {
//   "event_type": "payment.succeeded",
//   "data": { ... }
// }

// 6. Verify signature + process event
```

### Idempotent Retry

```javascript
// Network timeout on first request
try {
  await createPayment('order_12345', 1050, 'USD');
} catch (err) {
  // Retry with SAME idempotency key
  await createPayment('order_12345', 1050, 'USD');
  // Returns cached response, no duplicate charge
}
```

---

## Best Practices

### Idempotency Keys
- ✅ Use order ID or transaction ID
- ✅ Include timestamp if key might be reused: `order_12345_1705319445`
- ✅ Store key with order record for retry correlation
- ❌ Don't use random UUID (makes retry tracking impossible)

### Webhook Handling
- ✅ Verify signature BEFORE processing
- ✅ Return `200 OK` quickly (process async if needed)
- ✅ Use `Idempotency-Key` header to prevent duplicate processing
- ✅ Log webhook body for debugging
- ❌ Don't trust payload without signature verification

### Error Handling
- ✅ Check `success` field in response
- ✅ Parse `error_obj.code` for programmatic handling
- ✅ Implement exponential backoff for retries
- ✅ Log `error_obj.message` for debugging
- ❌ Don't retry `400 Bad Request` or `409 Conflict` errors

### Security
- ✅ Store API keys in environment variables
- ✅ Never commit API keys to version control
- ✅ Use HTTPS in production
- ✅ Rotate API keys periodically
- ❌ Don't log API keys or webhook signatures

---

## Support

For issues or questions about this API:
- Review documentation: `docs/architecture/ARCHITECTURE.md`
- Check verified claims: `docs/portfolio/verified-claims.md`
- See code examples: `docs/portfolio/upwork-case-study.md`

**Note:** This is a beta API. Production deployment requires compliance review and infrastructure hardening per README checklist.
