// Direct Link's panel: the pairing QR, as the Pi's airgapp.me page showed it. The QR is the link
// airgapp://car-host?host=<the car's tailnet address>; the iPhone camera opens it in the app, which asks
// "Connect to your car directly?" and then reaches agw there (serve -tailnet, port 17100). The address
// comes from $CSDK_RUN/panel/car-host.txt, which the package's supervisor keeps current from tailscale0.
// Nothing secret is shown: the car's key is pinned by the phone on its first connect. Plain ES5.
(function () {
  'use strict';

  var bridge = window.csdkPanel.createClient(window);
  var qr = document.getElementById('qr');
  var line = document.getElementById('line');
  var shown = null;
  var IPV4 = /^100\.(6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])\.[0-9]{1,3}\.[0-9]{1,3}$/;

  function show(host) {
    if (host === shown) return;
    shown = host;
    if (!host) {
      qr.hidden = true;
      qr.textContent = '';
      line.textContent = 'Waiting for Tailscale';
      return;
    }
    var code = qrcode(0, 'M');
    code.addData('airgapp://car-host?host=' + host);
    code.make();
    qr.innerHTML = code.createSvgTag({ cellSize: 8, margin: 2, scalable: true });
    qr.hidden = false;
    line.textContent = 'Scan with your iPhone camera';
  }

  function refresh() {
    bridge.data.get('car-host.txt').then(
      function (text) {
        var host = String(text || '').trim();
        show(IPV4.test(host) ? host : null);
      },
      function () {
        show(null);
      }
    );
  }

  bridge.applyChrome(document);
  refresh();
  window.setInterval(refresh, 5000);
})();
