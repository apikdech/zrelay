# How to watch the cameras: guide for the family

This guide has four steps. You must have two things: the web address of the viewer and a key.
The person who operates the system (the owner) gives you the two things. The address is a link.
The key is a text that starts with `zk1-`, or a QR picture.

## 1. Install Tailscale

The viewer is private. It opens only on devices in the family Tailscale network.

1. Install the Tailscale app from the App Store or Google Play.
2. Sign in with the account that the owner invited.
3. Turn on Tailscale. Keep it on when you watch.

## 2. Open the address

1. Open the address from the owner in Safari (iPhone) or Chrome (Android). The address looks
   like `https://mini.<name>.ts.net`.
2. Make sure that the zview page shows "Import your key first".

If the page does not open, make sure that Tailscale is on.

## 3. Add zview to your Home Screen

* **iPhone (Safari):** Tap Share. Tap **Add to Home Screen**. Then tap **Add**.
* **Android (Chrome):** Tap the menu (three dots). Then tap **Install app** or
  **Add to Home screen**.

From now on, open zview from the new icon.

> **NOTE:** On an iPhone, the Home Screen app has its own storage. This storage is separate from
> Safari. Thus, do step 4 in the Home Screen app, not in Safari.

## 4. Add the key

1. In zview, open the **Key** tab.
2. Do one of these two steps:
   - Paste the `zk1-...` text into **Master key**. Then tap **Import key**.
   - Tap **Scan QR code**. Let the app use the camera. Point the camera at the QR picture from
     the owner.
3. A short fingerprint shows. Make sure that it is the same as the fingerprint from the owner.
4. If the app shows "typo in key", the text has an error. Copy the key again and try again.
5. If the phone is your own phone, turn on **Remember this device**. On a shared phone, keep it
   off.

Now open **Timeline**. Select a day and tap a recording to watch it. **Live** shows the cameras
now.

## Important information

* The key never goes into a link, a message, or the address bar. Do not give the key to persons
  outside the family. A person with the key and Tailscale access can watch the cameras.
* To remove the key from a device, open **Key** and tap **Forget** next to the key. If you lose a
  phone, tell the owner immediately.
* When an update is available, the app shows "New version available — reload". Tap reload when
  no video plays.
* If a recording shows **tampered** or **incomplete**, somebody changed the file, or the file is
  not complete. Tell the owner.
