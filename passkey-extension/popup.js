const $ = (id) => document.getElementById(id);
let challengeLoaded = false;

function config() {
  const api = $("api").value.trim().replace(/\/$/, "");
  const token = $("token").value;
  const session = $("session").value.trim();
  if (!api.startsWith("http://127.0.0.1:") && !api.startsWith("https://")) {
    throw new Error("Use localhost ou um endereço HTTPS para a API.");
  }
  if (!token || !session) throw new Error("Informe o token e o ID da sessão.");
  return { api, token, session };
}

async function request(path, options = {}) {
  const { api, token, session } = config();
  const response = await fetch(`${api}/v1/sessions/${encodeURIComponent(session)}${path}`, {
    ...options,
    cache: "no-store",
    headers: {
      Authorization: `Bearer ${token}`,
      ...(options.body ? { "Content-Type": "application/json" } : {}),
      ...options.headers
    }
  });
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.error?.message || `A API respondeu HTTP ${response.status}.`);
  return body;
}

function setBusy(button, busy) {
  button.disabled = busy;
}

$("load").addEventListener("click", async () => {
  const button = $("load");
  setBusy(button, true);
  $("status").textContent = "Consultando desafio…";
  try {
    const result = await request("/passkey");
    if (!result.public_key) throw new Error("A API não retornou um desafio WebAuthn.");
    challengeLoaded = true;
    $("use").disabled = false;
    $("status").textContent = `Desafio disponível para ${result.public_key.relyingPartyId || result.public_key.relying_party_id || "o relying party informado pelo WhatsApp"}. Agora clique em “Usar passkey” com WhatsApp Web aberto.`;
  } catch (error) {
    challengeLoaded = false;
    $("use").disabled = true;
    $("status").textContent = error.message;
  } finally {
    setBusy(button, false);
  }
});

$("use").addEventListener("click", async () => {
  const button = $("use");
  setBusy(button, true);
  $("status").textContent = "Abra ou selecione a aba WhatsApp Web e aprove o pedido do autenticador…";
  try {
    if (!challengeLoaded) throw new Error("Busque um desafio antes de continuar.");
    const tab = await chrome.tabs?.query?.({ active: true, currentWindow: true }).then((tabs) => tabs[0]);
    if (!tab?.id) throw new Error("Não encontrei uma aba ativa do navegador.");
    if (!/^https:\/\/web\.whatsapp\.com\//.test(tab.url || "")) {
      throw new Error("Selecione uma aba aberta em https://web.whatsapp.com/.");
    }
    const { public_key: publicKey } = await request("/passkey");
    const execution = await chrome.scripting.executeScript({
      target: { tabId: tab.id },
      world: "MAIN",
      args: [publicKey],
      func: async (options) => {
        const decode = (value) => {
          const normalized = value.replace(/-/g, "+").replace(/_/g, "/");
          const raw = atob(normalized + "=".repeat((4 - normalized.length % 4) % 4));
          return Uint8Array.from(raw, (char) => char.charCodeAt(0));
        };
        const encode = (value) => {
          const bytes = new Uint8Array(value);
          let raw = "";
          for (const byte of bytes) raw += String.fromCharCode(byte);
          return btoa(raw).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/g, "");
        };
        const challenge = { ...options, challenge: decode(options.challenge) };
        if (challenge.allowCredentials) {
          challenge.allowCredentials = challenge.allowCredentials.map((item) => ({ ...item, id: decode(item.id) }));
        }
        const credential = await navigator.credentials.get({ publicKey: challenge });
        if (!credential || !(credential.response instanceof AuthenticatorAssertionResponse)) {
          throw new Error("O autenticador não retornou uma asserção passkey.");
        }
        return {
          id: credential.id,
          rawId: encode(credential.rawId),
          type: credential.type,
          authenticatorAttachment: credential.authenticatorAttachment,
          clientExtensionResults: credential.getClientExtensionResults(),
          response: {
            clientDataJSON: encode(credential.response.clientDataJSON),
            authenticatorData: encode(credential.response.authenticatorData),
            signature: encode(credential.response.signature),
            userHandle: credential.response.userHandle ? encode(credential.response.userHandle) : null
          }
        };
      }
    });
    const assertion = execution[0]?.result;
    if (!assertion) throw new Error("O navegador não retornou uma resposta do autenticador.");
    await request("/passkey", { method: "POST", body: JSON.stringify(assertion) });
    challengeLoaded = false;
    $("use").disabled = true;
    $("status").textContent = "Resposta enviada. Se o WhatsApp solicitar conferência, consulte o código abaixo.";
  } catch (error) {
    $("status").textContent = error.message || "O navegador cancelou ou recusou o pedido passkey.";
  } finally {
    setBusy(button, false);
  }
});

$("check").addEventListener("click", async () => {
  const button = $("check");
  setBusy(button, true);
  try {
    const result = await request("/passkey/confirmation");
    $("confirmPanel").style.display = "block";
    $("code").textContent = result.code;
    $("typedCode").value = "";
    $("confirm").disabled = !result.confirmation_required;
    $("status").textContent = result.confirmation_required
      ? "Confirme somente depois de comparar os códigos no WhatsApp e aqui."
      : "O WhatsApp está processando a confirmação automaticamente.";
  } catch (error) {
    $("confirmPanel").style.display = "none";
    $("status").textContent = error.message;
  } finally {
    setBusy(button, false);
  }
});

$("confirm").addEventListener("click", async () => {
  const button = $("confirm");
  setBusy(button, true);
  try {
    const expected = $("code").textContent;
    const entered = $("typedCode").value.trim();
    if (!entered || entered !== expected) throw new Error("O código digitado não corresponde ao código exibido.");
    await request("/passkey/confirmation", { method: "POST", body: JSON.stringify({ code: entered, confirm: true }) });
    $("confirmPanel").style.display = "none";
    $("status").textContent = "Confirmação enviada. Consulte a sessão para verificar quando ficar conectada.";
  } catch (error) {
    $("status").textContent = error.message;
  } finally {
    setBusy(button, false);
  }
});
