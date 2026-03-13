# Semantic Events

Segment defines seven standardized semantic event categories with reserved event names and properties. These categories provide semantic meaning to Track events, enabling destination connectors to map event names to platform-specific actions (e.g., `Order Completed` → Google Analytics Enhanced Ecommerce purchase).

RudderStack supports all seven semantic event categories with **full pass-through behavior** — all event names are accepted and forwarded through the pipeline as opaque strings without modification, validation, or rejection. Destination-specific mapping is handled by the external Transformer service (`rudder-transformer`) during the destination transformation stage, not by the Gateway or Processor.

> **Source references:**
>
> - `refs/segment-docs/src/connections/spec/ecommerce/v2.md` — Segment E-Commerce v2 semantic events
> - `refs/segment-docs/src/connections/spec/video.md` — Segment Video semantic events
> - `refs/segment-docs/src/connections/spec/mobile.md` — Segment Mobile lifecycle semantic events
> - `processor/internal/transformer/destination_transformer/` — Destination transformation orchestration

> **Important:** Not all destinations support every semantic event. Refer to individual destination documentation for supported events and properties. Semantic event mapping is performed by the Transformer service at port 9090.

---

## Pass-Through Behavior

RudderStack's Gateway and Processor treat all event names as opaque strings:

1. **Gateway** — Accepts any `event` property value in Track calls without validation against a reserved event name list. No event names are rejected or modified.
2. **Processor** — Passes all event names through the 6-stage pipeline (preprocess → source hydration → pre-transform → user transform → destination transform → store) without modification.
3. **Transformer** — The external Transformer service (`rudder-transformer`) performs destination-specific mapping during the destination transformation stage. For example, `Order Completed` is mapped to Google Analytics Enhanced Ecommerce `purchase` action.
4. **Router** — Delivers transformed payloads to destinations with all semantic properties preserved.

This pass-through approach is **identical to Segment's behavior** — Segment also accepts all event names at the API level and performs semantic mapping at the destination level.

---

## E-Commerce v2

The E-Commerce v2 semantic event category covers the complete customer journey from browsing through purchase. These events enable destinations like Google Analytics Enhanced Ecommerce, Facebook Pixel, and advertising platforms to automatically map to platform-specific conversion events.

Source: `refs/segment-docs/src/connections/spec/ecommerce/v2.md`

### Browsing Events

| Event Name | Description |
|---|---|
| `Products Searched` | User searched for products |
| `Product List Viewed` | User viewed a product list or category |
| `Product List Filtered` | User filtered a product list or category |

### Promotion Events

| Event Name | Description |
|---|---|
| `Promotion Viewed` | User viewed a promotion |
| `Promotion Clicked` | User clicked on a promotion |

### Core Ordering Events

| Event Name | Description |
|---|---|
| `Product Clicked` | User clicked on a product |
| `Product Viewed` | User viewed product details |
| `Product Added` | User added a product to their shopping cart |
| `Product Removed` | User removed a product from their shopping cart |
| `Cart Viewed` | User viewed their shopping cart |
| `Checkout Started` | User initiated the order process (a transaction is created) |
| `Checkout Step Viewed` | User viewed a checkout step |
| `Checkout Step Completed` | User completed a checkout step |
| `Payment Info Entered` | User added payment information |
| `Order Completed` | User completed the order |
| `Order Updated` | User updated the order |
| `Order Refunded` | User refunded the order |
| `Order Cancelled` | User cancelled the order |

### Coupon Events

| Event Name | Description |
|---|---|
| `Coupon Entered` | User entered a coupon on a shopping cart or order |
| `Coupon Applied` | Coupon was applied on a user's shopping cart or order |
| `Coupon Denied` | Coupon was denied from a user's shopping cart or order |
| `Coupon Removed` | User removed a coupon from a cart or order |

### Wishlist Events

| Event Name | Description |
|---|---|
| `Product Added to Wishlist` | User added a product to the wish list |
| `Product Removed from Wishlist` | User removed a product from the wish list |
| `Wishlist Product Added to Cart` | User added a wishlist product to the cart |

### Sharing Events

| Event Name | Description |
|---|---|
| `Product Shared` | Shared a product with one or more friends |
| `Cart Shared` | Shared the cart with one or more friends |

### Review Events

| Event Name | Description |
|---|---|
| `Product Reviewed` | User reviewed a product |

> **Destination Mapping Example:** When `Order Completed` is sent as a Track event, the Transformer maps it to Google Analytics Enhanced Ecommerce `purchase` action, Facebook Pixel `Purchase` event, and other platform-specific purchase events. The mapping is configured per destination in the `rudder-transformer` service.

---

## Video

The Video semantic event category defines how customers engage with video and ad content. Video events are organized into four sub-categories: Playback (player-level session events), Content (content-level events), Ads (advertisement events), and Quality (performance monitoring).

Source: `refs/segment-docs/src/connections/spec/video.md`

> **Session Binding:** All video events use a shared `session_id` property to tie playback, content, and ad events together within a single viewing session. If a web page has two video players, each player produces a separate session with its own `session_id`.

### Playback Events

Playback events track the state of the video player at the session level. These events fire based on user actions (play, pause, seek) and player state changes (buffering, interruption).

| Event Name | Description |
|---|---|
| `Video Playback Started` | User pressed play; fires after the last user action required for playback to begin |
| `Video Playback Paused` | User pressed pause |
| `Video Playback Interrupted` | Playback stopped unintentionally (network loss, browser close/redirect, app crash) |
| `Video Playback Buffer Started` | Playback started buffering content or an ad |
| `Video Playback Buffer Completed` | Playback finished buffering content or an ad |
| `Video Playback Seek Started` | User manually sought a certain position in the content or ad |
| `Video Playback Seek Completed` | User completed seeking to a certain position in the content or ad |
| `Video Playback Resumed` | Playback resumed by the user after being paused |
| `Video Playback Completed` | Playback completed and the session is finished |
| `Video Playback Exited` | User navigated away from the playback/stream |

### Content Events

Content events track user interaction with video content segments within a playback session. A single playback session may contain multiple content pods (segments) if interrupted by mid-roll ads.

| Event Name | Description |
|---|---|
| `Video Content Started` | A video content segment started playing within a playback |
| `Video Content Playing` | Heartbeat event fired every N seconds to track playback position within content |
| `Video Content Completed` | A video content segment completed playing within a playback |

### Ad Events

Ad events track user interaction with advertisements within a playback session. A playback session may contain multiple ad pods (pre-roll, mid-roll, post-roll), each with one or more ad assets.

| Event Name | Description |
|---|---|
| `Video Ad Started` | An ad started playing within a playback |
| `Video Ad Playing` | Heartbeat event fired every N seconds to track playback position within an ad |
| `Video Ad Completed` | An ad completed playing within a playback |

### Quality Events

Quality events track video performance metrics for monitoring and optimization.

| Event Name | Description |
|---|---|
| `Video Quality Updated` | Video quality changed during playback (tracks `bitrate`, `framerate`, `startupTime`, `droppedFrames`) |

---

## Mobile

The Mobile semantic event category standardizes the core mobile application lifecycle and associated campaign/referral events. By using these standardized event names, destination connectors can automatically map to platform-specific features such as Facebook dynamic ads and mobile attribution platforms.

Source: `refs/segment-docs/src/connections/spec/mobile.md`

> **Auto-Collection:** RudderStack's mobile SDKs can automatically collect lifecycle events (`Application Installed`, `Application Opened`, `Application Updated`) when lifecycle tracking is enabled. The Swift library additionally auto-tracks `Application Backgrounded` and `Application Foregrounded`. Campaign events are not automatically collected unless explicitly configured.

### Lifecycle Events

Lifecycle events track the core flows associated with installing, opening, updating, and closing a mobile application. These events provide top-line metrics such as DAUs, MAUs, and screen views per session.

| Event Name | Description |
|---|---|
| `Application Installed` | First open of the mobile application. Does not wait for attribution data. |
| `Application Opened` | Subsequent launches or foregrounds after the first open. Fires after `Application Installed` and on re-opens. |
| `Application Backgrounded` | App moved to background (collected automatically by Kotlin, Swift, and React Native libraries) |
| `Application Foregrounded` | App returned to foreground (auto-tracked by Swift library only) |
| `Application Updated` | Fires when a version change is detected on open. Sent instead of `Application Opened` on first launch after update. |
| `Application Uninstalled` | App uninstalled. Detected via silent push notifications by destination partners. |
| `Application Crashed` | App crash detected. Not meant to supplant traditional crash reporting tools. |

### Campaign Events

Campaign events capture information about the content and campaigns that drive users to engage with the mobile application, enabling more targeted and personalized experiences.

| Event Name | Description |
|---|---|
| `Install Attributed` | Attribution data received from a provider (Tune, Kochava, Branch, AppsFlyer). Fires after install. |
| `Push Notification Received` | Push notification delivered to the device. Can be automatically enabled on iOS. |
| `Push Notification Tapped` | User tapped a push notification associated with the app. Can be automatically enabled on iOS. |
| `Push Notification Bounced` | Push notification could not be delivered to the device |
| `Deep Link Opened` | App opened via a referring deep link. Fires in addition to `Application Opened`. |
| `Deep Link Clicked` | Deep link click postback received from a deep link provider or internal redirect service |

---

## B2B SaaS

The B2B SaaS semantic event category covers account-level and subscription lifecycle events commonly used in business-to-business software products.

| Event Name | Description |
|---|---|
| `Account Created` | A new account or organization was created |
| `Account Deleted` | An account or organization was deleted |
| `Trial Started` | A free trial period was initiated |
| `Trial Ended` | A free trial period ended |
| `Signed Up` | A user completed the sign-up process |
| `Signed In` | A user signed in to their account |
| `Signed Out` | A user signed out of their account |
| `Invite Sent` | A user sent an invitation to another user |

These event names follow the same pass-through pattern — RudderStack accepts, preserves, and forwards them without modification. Destination-specific handling is managed by the Transformer service.

---

## Email

The Email semantic event category covers email delivery and engagement lifecycle events. These events are typically generated by email service providers and forwarded through RudderStack for unified analytics.

| Event Name | Description |
|---|---|
| `Email Bounced` | Email could not be delivered to the recipient |
| `Email Delivered` | Email was successfully delivered to the recipient's inbox |
| `Email Link Clicked` | Recipient clicked a link within the email |
| `Email Marked as Spam` | Recipient marked the email as spam |
| `Email Opened` | Recipient opened the email |
| `Email Unsubscribed` | Recipient unsubscribed from the email list |

These event names follow the same pass-through pattern — RudderStack accepts, preserves, and forwards them without modification. Destination-specific handling is managed by the Transformer service.

---

## Live Chat

The Live Chat semantic event category covers real-time messaging and customer support interactions. These events are typically generated by live chat platforms and forwarded through RudderStack for unified analytics.

| Event Name | Description |
|---|---|
| `Live Chat Conversation Started` | A new live chat conversation was initiated |
| `Live Chat Conversation Ended` | A live chat conversation was closed or ended |
| `Live Chat Message Sent` | A message was sent in a live chat conversation |
| `Live Chat Message Received` | A message was received in a live chat conversation |

These event names follow the same pass-through pattern — RudderStack accepts, preserves, and forwards them without modification. Destination-specific handling is managed by the Transformer service.

---

## A/B Testing

The A/B Testing semantic event category covers experiment and variation exposure events. These events are typically generated by experimentation platforms and forwarded through RudderStack for unified analytics.

| Event Name | Description |
|---|---|
| `Experiment Viewed` | User was exposed to an experiment variation |

These event names follow the same pass-through pattern — RudderStack accepts, preserves, and forwards them without modification. Destination-specific handling is managed by the Transformer service.

---

## Segment Behavioral Parity

The following table summarizes RudderStack's semantic event parity with the Segment Spec across all seven categories:

| Category | Event Count | RudderStack Behavior | Parity Status |
|---|---|---|---|
| E-Commerce v2 | 28 events | Full pass-through; destination mapping via Transformer | ✅ Full Parity |
| Video | 17 events | Full pass-through; destination mapping via Transformer | ✅ Full Parity |
| Mobile | 13 events | Full pass-through; auto-collected by mobile SDKs | ✅ Full Parity |
| B2B SaaS | Category-level | Full pass-through | ✅ Full Parity |
| Email | Category-level | Full pass-through | ✅ Full Parity |
| Live Chat | Category-level | Full pass-through | ✅ Full Parity |
| A/B Testing | Category-level | Full pass-through | ✅ Full Parity |

> **Migration Note:** When migrating from Segment to RudderStack, no changes are required for semantic event names or properties. All event names are accepted identically. Destination-specific mapping is handled by the Transformer service.

---

## See Also

- [Track](track.md) — Track call specification (semantic events are sent via Track)
- [Common Fields](common-fields.md) — Common fields shared across all event types
- [RudderStack Extensions](extensions.md) — Extension endpoints beyond the Segment Spec
- [Gateway HTTP API](../gateway-http-api.md) — Full HTTP API reference
- [API Overview & Authentication](../index.md) — Authentication guide
