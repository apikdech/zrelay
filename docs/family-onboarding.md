# Watching the cameras: family guide

One page, four steps. You need the web address of the viewer and a key. The
person who runs the system (the owner) gives you both: the address as a link,
the key as a text starting with `zk1-` or as a QR picture.

## 1. Install Tailscale

The viewer is private. It only opens on devices that are in the family
Tailscale network.

1. Install the Tailscale app from the App Store or Google Play.
2. Sign in with the account the owner invited you with, and switch Tailscale
   on. Leave it on whenever you want to watch.

## 2. Open the address

Open the address the owner sent (it looks like `https://mini.<name>.ts.net`)
in Safari on iPhone or Chrome on Android. You should see the zview page and
"Import your key first". If the page does not load, check that Tailscale is on.

## 3. Add it to your Home Screen

* **iPhone (Safari):** tap Share, then **Add to Home Screen**, then Add.
* **Android (Chrome):** tap the menu (three dots), then **Install app** or
  **Add to Home screen**.

Open zview from the new icon from now on. On an iPhone the Home Screen app
keeps its own storage, separate from Safari, so do step 4 inside the Home
Screen app, not in Safari.

## 4. Add the key (paste or scan the QR)

1. In zview open **Keys**.
2. Either paste the `zk1-...` text into **Master key** and tap **Import key**,
   or tap **Scan QR code**, allow the camera, and point it at the QR picture the
   owner showed you.
3. A short fingerprint appears. Check it matches the one the owner told you.
   If the app says "typo in key", the text was copied wrongly: try again.
4. Switch on **Remember this device** if it is your own phone. Leave it off on
   a shared one.

Now open **Timeline**, pick a day and tap a recording to watch it. **Live**
shows the cameras right now.

## Good to know

* The key never goes in a link, a message to the app, or the address bar. Do
  not share it outside the family; anyone with the key and Tailscale access can
  watch.
* Lost your phone, or want to stop watching on a device? Open **Keys** and tap
  **Forget key ... on this device**. Tell the owner if a phone is lost.
* If an app update is ready you will see "New version available — reload".
  Tap reload when you are not in the middle of a video.
* A recording marked **tampered** or **incomplete** means the file was changed
  or cut short; tell the owner.
