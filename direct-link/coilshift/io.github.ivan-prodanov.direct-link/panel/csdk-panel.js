// The panel side of the CoilShift bridge (SDK design 3.6, option B). A panel is a page the CoilShift app hosts in
// <iframe sandbox="allow-scripts">: it has an opaque origin, no cookies and no access to /api. It talks to the app
// with postMessage and the app answers what the panel's manifest permissions allow.
//
//   request   {"csdk":1,"id":n,"method":m,"params":{...}}      posted to window.parent with target origin "*"
//   answer    {"csdk":1,"id":n,"result":...}  or  {"csdk":1,"id":n,"error":{"code":...,"message":...}}
//   event     {"csdk":1,"event":"settings"|"status"}           only after events.subscribe
//
// Only the app's window is listened to: a message whose source is not window.parent is dropped. Vendor this file
// byte for byte (the SDK's test checks the copy). Plain ES5 so it runs on the car's Chromium without a build step.
(function (root) {
  'use strict';

  var DEFAULT_TIMEOUT_MS = 10000;

  function BridgeError(code, message) {
    var error = new Error(message || code);
    error.name = 'BridgeError';
    error.code = code;
    return error;
  }

  // settings.get answers the settings in scope as a list of views ({id, type, title, value, choices, ...}), or an
  // object holding that list as `settings`; both are read here so a panel sees a list.
  function settingList(result) {
    if (Array.isArray(result)) return result;
    if (result && Array.isArray(result.settings)) return result.settings;
    return [];
  }

  // A setting by its full ID or by the name after the package ID, so a panel does not hard-code its own package ID.
  function findSetting(list, name) {
    for (var i = 0; i < list.length; i++) {
      var id = list[i].id || '';
      if (id === name || id.slice(-(name.length + 1)) === '.' + name) return list[i];
    }
    return null;
  }

  function createClient(win, options) {
    var parent = win.parent;
    var timeoutMs = (options && options.timeoutMs) || DEFAULT_TIMEOUT_MS;
    var pending = {};
    var nextId = 1;
    var handlers = { settings: [], status: [] };
    var subscribed = false;

    function onMessage(event) {
      var message = event && event.data;
      if (!message || message.csdk !== 1 || event.source !== parent) return;
      if (typeof message.event === 'string') {
        var list = handlers[message.event] || [];
        for (var i = 0; i < list.length; i++) list[i]();
        return;
      }
      var entry = Object.prototype.hasOwnProperty.call(pending, message.id) ? pending[message.id] : null;
      if (!entry) return;
      delete pending[message.id];
      win.clearTimeout(entry.timer);
      if (message.error) entry.reject(BridgeError(message.error.code, message.error.message));
      else entry.resolve(message.result);
    }
    win.addEventListener('message', onMessage);

    function request(method, params) {
      return new Promise(function (resolve, reject) {
        var id = nextId++;
        var timer = win.setTimeout(function () {
          delete pending[id];
          reject(BridgeError('timeout', method + ' got no answer from the app'));
        }, timeoutMs);
        pending[id] = { resolve: resolve, reject: reject, timer: timer };
        parent.postMessage({ csdk: 1, id: id, method: method, params: params || {} }, '*');
      });
    }

    function on(name, handler) {
      if (!handlers[name]) throw new Error('no such event: ' + name);
      handlers[name].push(handler);
      if (!subscribed) {
        subscribed = true;
        request('events.subscribe').catch(function () {
          subscribed = false;
        });
      }
      return function off() {
        handlers[name] = handlers[name].filter(function (candidate) {
          return candidate !== handler;
        });
      };
    }

    return {
      request: request,
      on: on,
      settings: {
        get: function () {
          return request('settings.get').then(settingList);
        },
        set: function (id, value) {
          return request('settings.set', { id: id, value: value });
        },
      },
      status: function () {
        return request('status');
      },
      data: {
        // The text of $CSDK_RUN/panel/<name> on the car.
        get: function (name) {
          return request('data.get', { name: name }).then(function (result) {
            return typeof result === 'string' ? result : result && typeof result.text === 'string' ? result.text : '';
          });
        },
      },
      theme: function () {
        return request('theme').then(function (result) {
          return result && result.mode === 'day' ? 'day' : 'night';
        });
      },
      fonts: function () {
        return request('fonts').then(function (result) {
          return typeof result === 'string' ? result : result && result.url ? result.url : '';
        });
      },
      // Theme and fonts as the touchscreen has them: data-theme on <html> and the font stylesheet, best effort.
      applyChrome: function (doc) {
        var themed = this.theme().then(function (mode) {
          doc.documentElement.setAttribute('data-theme', mode === 'day' ? 'light' : 'dark');
        });
        var fonted = this.fonts().then(function (url) {
          if (!url) return;
          var link = doc.createElement('link');
          link.setAttribute('rel', 'stylesheet');
          link.setAttribute('href', url);
          doc.head.appendChild(link);
        });
        return Promise.all([
          themed.catch(function () {}),
          fonted.catch(function () {}),
        ]);
      },
      close: function () {
        win.removeEventListener('message', onMessage);
      },
    };
  }

  var api = { createClient: createClient, findSetting: findSetting, settingList: settingList };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else root.csdkPanel = api;
})(typeof window !== 'undefined' ? window : this);
