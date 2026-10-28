# Payment Gateway — Architecture Diagrams

This directory contains Mermaid diagram source files for the payment gateway architecture.

## Diagram Files

### Recommended for Upwork Portfolio (Ranked by Impact):

1. **`payment-system-architecture.mmd`** ⭐ **HERO IMAGE**
   - Complete system architecture
   - Shows breadth: API, services, workers, external integrations
   - Use as main/first screenshot

2. **`idempotency-protection.mmd`**
   - Multi-layer idempotency protection
   - Shows edge case handling: race conditions, retries
   - Demonstrates distributed systems expertise

3. **`double-entry-ledger.mmd`**
   - Financial ledger implementation
   - Shows domain expertise: accounting principles
   - Validation rules and database constraints

4. **`webhook-security.mmd`**
   - HMAC-SHA256 verification flow
   - Shows security consciousness
   - Inbox pattern and async processing

### Additional Diagrams:

5. **`payment-lifecycle.mmd`**
   - Payment state machine
   - Status transitions with ledger entries

6. **`webhook-retry-flow.mmd`**
   - Webhook retry logic
   - Exponential backoff visualization

7. **`financial-transaction.mmd`**
   - Atomic transaction boundaries
   - PostgreSQL transaction flow

8. **`payment-provider-abstraction.mmd`**
   - Strategy Pattern implementation
   - Provider interface and implementations

9. **`database-model.mmd`**
   - Entity-relationship diagram
   - Key tables and relationships

10. **`outbound-webhook-delivery.mmd`**
    - Merchant webhook delivery flow
    - Exponential backoff schedule

11. **`deployment-architecture.mmd`**
    - Docker/Kubernetes deployment
    - Infrastructure components

---

## Converting to Images

### Option 1: Mermaid CLI (Recommended)

Install Mermaid CLI:
```bash
npm install -g @mermaid-js/mermaid-cli
```

Convert single diagram:
```bash
mmdc -i payment-system-architecture.mmd -o payment-system-architecture.png -w 1920 -H 1080 -b white
```

Convert all diagrams:
```bash
for file in *.mmd; do
    mmdc -i "$file" -o "${file%.mmd}.png" -w 1920 -H 1080 -b white
done
```

**Recommended Settings:**
- Width: 1920px (high resolution)
- Height: Auto or 1080px
- Background: white
- Format: PNG

---

### Option 2: Online Mermaid Editor

1. Visit: https://mermaid.live/
2. Paste diagram content
3. Click "Actions" → "Download PNG" or "Download SVG"
4. Set width to 1920px for high resolution

---

### Option 3: VS Code Extension

Install "Markdown Preview Mermaid Support":
```
ext install bierner.markdown-mermaid
```

Then:
1. Open `.mmd` file in VS Code
2. Right-click diagram in preview
3. "Copy Image" or "Save Image"

---

### Option 4: GitHub (Quick Preview)

1. Rename `.mmd` to `.md`
2. Push to GitHub
3. View in GitHub's Markdown renderer
4. Take screenshot (use browser's full-page screenshot)

---

## Image Quality Settings for Upwork

### For Main Portfolio Screenshot:
- **Resolution:** 1920x1080 or higher
- **Format:** PNG (lossless)
- **DPI:** 300 (print quality)
- **File Size:** Under 5MB (Upwork limit)

### For Thumbnail Preview:
- **Resolution:** 800x600
- **Format:** PNG or JPEG
- **Ensure text is readable at thumbnail size**

### Upwork Image Requirements:
- Max file size: 5MB
- Recommended dimensions: 780px × 585px minimum
- Accepted formats: JPG, PNG, GIF
- Recommended: 1920x1080 for clarity

---

## Diagram Recommendations by Use Case

### For System Architecture / Backend Engineering Roles:
1. `payment-system-architecture.mmd` (breadth)
2. `financial-transaction.mmd` (database expertise)
3. `payment-provider-abstraction.mmd` (design patterns)

### For Fintech / Financial Systems Roles:
1. `double-entry-ledger.mmd` (financial domain)
2. `payment-lifecycle.mmd` (payment flow)
3. `idempotency-protection.mmd` (reliability)

### For Security-Focused Roles:
1. `webhook-security.mmd` (crypto + verification)
2. `idempotency-protection.mmd` (attack prevention)
3. `payment-system-architecture.mmd` (defense in depth)

### For Distributed Systems Roles:
1. `idempotency-protection.mmd` (race conditions)
2. `webhook-retry-flow.mmd` (retry logic)
3. `outbound-webhook-delivery.mmd` (exponential backoff)

---

## Visual Tips

### Making Diagrams Portfolio-Ready:

1. **Ensure text is readable:**
   - Minimum font size: 12pt
   - Use high contrast (black text on white background)
   - Avoid tiny labels

2. **Keep it concise:**
   - 5-12 major nodes per diagram
   - Too many nodes = unreadable
   - Split complex flows into multiple diagrams

3. **Use consistent styling:**
   - Color coding: green = success, red = error, yellow = processing
   - Box shapes: rectangles = services, cylinders = databases, clouds = external

4. **Add context notes:**
   - Brief explanations in note boxes
   - Code snippets where helpful
   - Security guarantees

5. **Test at thumbnail size:**
   - Shrink to 200x150px
   - Can you still read the main labels?
   - If not, reduce detail or increase font size

---

## Editing Diagrams

All diagrams use Mermaid syntax. To edit:

1. Open `.mmd` file in text editor
2. Modify Mermaid code
3. Preview using one of the methods above
4. Re-export to PNG

### Mermaid Syntax Reference:
- **Flowcharts:** `graph TB` or `graph LR`
- **Sequence Diagrams:** `sequenceDiagram`
- **State Diagrams:** `stateDiagram-v2`
- **Class Diagrams:** `classDiagram`
- **ER Diagrams:** `erDiagram`

Documentation: https://mermaid.js.org/

---

## Portfolio Assembly Checklist

- [ ] Convert top 4 diagrams to PNG (1920x1080)
- [ ] Verify text is readable at thumbnail size
- [ ] Ensure file sizes under 5MB each
- [ ] Test display on Upwork (upload to draft)
- [ ] Add diagram to portfolio with description
- [ ] Link to GitHub repo for full documentation

---

## Questions?

All diagrams are generated from verified code inspection. Every component shown exists in the codebase and is referenced in:

- `docs/portfolio/verified-claims.md` — Claims verification
- `docs/architecture/ARCHITECTURE.md` — Technical details
- `docs/AUDIT-SUMMARY.md` — Complete audit

**The code is the source of truth.**
