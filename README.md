# WhatsApp Go API

API HTTP em Go sobre [`go.mau.fi/whatsmeow`](https://github.com/tulir/whatsmeow). Gerencia várias contas WhatsApp independentes no mesmo processo: cada sessão tem seu próprio dispositivo, credenciais, conexão, eventos, contatos, grupos e operações. Inclui pareamento QR e por telefone, mensagens de texto e mídia, presença, leitura e webhooks. Todo o sistema usa diretamente o protocolo do WhatsApp via whatsmeow — não há dependência da Cloud API da Meta nem de serviços de terceiros como a Evolution API.

> `whatsmeow` é uma integração não oficial. Mudanças do protocolo podem interromper conexões e o uso pode estar sujeito a restrições do WhatsApp. Use somente contas e dados que você está autorizado a controlar.

## Implantação na nuvem

O InovarApp usa um único processo persistente no Render e uma única sessão WhatsApp ativa por vez. As chaves whatsmeow, histórico, produtos, fila de webhooks e anexos ficam no schema privado `whatsapp` do Supabase Postgres. O runtime não usa SQLite nem disco local para persistência.

Configure no serviço Go do Render:

- `API_TOKEN`: segredo aleatório com pelo menos 32 caracteres; use o mesmo valor em `WHATSAPP_OWN_TOKEN` no backend da Vercel.
- `DATABASE_DSN`: URL Postgres do Supabase com TLS (`sslmode=require`), guardada como segredo no Render.
- `PORT`: porta fornecida pelo Render. O serviço escuta em `0.0.0.0:$PORT`.

No backend do InovarApp, configure `WHATSAPP_OWN_URL` com o endereço público do serviço Go, `WHATSAPP_OWN_TOKEN` com o token acima e `WHATSAPP_OWN_SESSION=inovar`. Antes de iniciar a API, aplique `supabase/migrations/20261005_whatsapp_go_postgres.sql`. O serviço verifica se o schema privado existe e recusa iniciar se estiver ausente.

Na primeira conexão, o app cria a sessão automaticamente, inicia o pareamento e acompanha o estado até receber o QR ou código. O código por telefone vincula um aparelho; não é um SMS/OTP. No WhatsApp do celular, abra **Aparelhos conectados > Conectar aparelho > Conectar com número de telefone** e digite o código mostrado no app. Para trocar de número, remova a sessão conectada no app e então inicie uma nova conexão.

`whatsmeow` implementa o protocolo não oficial do WhatsApp e depende de compatibilidade contínua com mudanças do serviço. A opção Meta Cloud API usa outro fluxo de ativação Business e precisa de credenciais e configuração próprias; não é ativada por QR ou código de telefone.
## Autenticação e limites

Todas as rotas `/v1/*` exigem `Authorization: Bearer <API_TOKEN>`. `/healthz` e `/readyz` são públicos. Há limite por IP: 60 chamadas por minuto na maioria das rotas e rajada de cinco chamadas de conexão/pareamento, repondo uma a cada 12 segundos. O limite é aplicado antes da autenticação, então chamadas com token inválido também consomem a cota do IP.

Não registre tokens, códigos QR ou texto completo em logs. Webhooks de mensagem incluem texto recebido e eventos QR incluem código de pareamento. Restrinja o acesso ao destino e ao banco. Webhooks aceitam apenas HTTPS, bloqueiam IPs privados/reservados e não seguem redirecionamentos.

## Rotas

| Método e rota | Uso |
|---|---|
| `GET /` | Painel administrativo do serviço: criar/conectar sessões, ver QR, parear por telefone, enviar mensagem de teste |
| `GET /healthz` | Processo vivo |
| `GET /readyz` | Confirma acesso ao banco |
| `POST /v1/sessions` | Cria uma conta/sessão independente; JSON opcional `{"id":"loja-a"}`; repita para cada conta |
| `GET /v1/sessions` | Lista todas as contas/sessões com ID, estado e JID da conta pareada |
| `GET /v1/sessions/{id}` | Consulta estado e JID da conta daquela sessão |
| `POST /v1/sessions/{id}/connect` | Inicia conexão para pareamento e retorna o estado atual de uma sessão já conectada ao servidor |
| `GET /v1/sessions/{id}/pairing` | Obtém o texto QR atual como JSON |
| `GET /v1/sessions/{id}/pairing?format=png` | Obtém o QR atual como imagem PNG privada e sem cache |
| `POST /v1/sessions/{id}/pair` | Gera código de pareamento; JSON `{"phone":"5511999999999"}` após iniciar conexão |
| `GET /v1/sessions/{id}/passkey` | Obtém o desafio WebAuthn pendente, se o WhatsApp solicitar passkey |
| `POST /v1/sessions/{id}/passkey` | Encaminha a resposta WebAuthn produzida pelo autenticador do titular |
| `GET /v1/sessions/{id}/passkey/confirmation` | Obtém o código que o titular deve conferir no WhatsApp |
| `POST /v1/sessions/{id}/passkey/confirmation` | Confirma o pareamento após a conferência humana do código |
| `POST /v1/sessions/{id}/disconnect` | Desconecta e mantém credenciais |
| `DELETE /v1/sessions/{id}` | Faz logout ou apaga credenciais locais e remove sessão |
| `POST /v1/sessions/{id}/messages` | Texto JSON ou mídia multipart |
| `POST /v1/sessions/{id}/messages/{message_id}/read` | JSON `{"chat":"...","sender":"..."}`; sender opcional |
| `POST /v1/sessions/{id}/presence` | `typing`, `recording` ou `paused`, com chat opcional |
| `GET /v1/sessions/{id}/contacts` | Lista contatos disponíveis no cache local |
| `GET /v1/sessions/{id}/groups` | Consulta grupos associados à sessão |
| `GET /v1/sessions/{id}/chats/{jid}/settings` | Consulta preferências locais de arquivo, fixação e silenciamento |
| `PATCH /v1/sessions/{id}/chats/{jid}/settings` | Atualiza uma ou mais dessas preferências |
| `GET /v1/sessions/{id}/chats` | Lista conversas com última mensagem e contagem |
| `GET /v1/sessions/{id}/chats/{jid}` | Histórico de mensagens da conversa (recebidas e enviadas) |
| `GET /v1/sessions/{id}/products` | Lista produtos do catálogo local da sessão |
| `POST /v1/sessions/{id}/products` | Cadastra produto no catálogo local (JSON `name`, `price`, `sku`, `description`, `image_url`, `currency`) |
| `PATCH /v1/sessions/{id}/products/{product_id}` | Atualiza campos de um produto |
| `DELETE /v1/sessions/{id}/products/{product_id}` | Exclui produto |
| `GET /v1/media/{filename}` | Baixa um anexo recebido, autenticado |

### Catálogo local de produtos

O catálogo é próprio deste sistema e fica no Postgres privado do Supabase — não é o catálogo do app WhatsApp Business (que só pode ser gerenciado no próprio aplicativo ou pela Cloud API da Meta; o protocolo não permite cadastrá-lo remotamente). Preço é texto simples tipo `79.90` com moeda ISO (padrão BRL) e a imagem é opcional por HTTPS.

### Painel web do serviço

Abrir a raiz do serviço mostra um painel administrativo: escolha a sessão no topo e opere **Conversas**, **Contatos** e **Catálogo**. Em **Gerenciar sessões** ficam criação, QR, código por telefone, desconexão e remoção. O painel deve ser protegido pelo token de API; não o publique sem autenticação.

O painel não recebe token automaticamente em produção. Informe o token administrativo somente em uma origem confiável. A página é estática e sem segredos próprios. A etapa passkey exige suporte WebAuthn no contexto de origem aceito pelo desafio.

Cada rota de operação inclui o ID da sessão. Assim, por exemplo, `/v1/sessions/loja-a/messages` envia usando somente a conta `loja-a`, enquanto `/v1/sessions/loja-b/messages` usa a conta `loja-b`. Crie, conecte e pareie cada sessão separadamente. As sessões pareadas são reconectadas na inicialização; desconectar mantém credenciais e excluir uma sessão remove somente as credenciais daquela conta.

Sessões pareadas tentam reconectar automaticamente após falhas temporárias de rede, inclusive durante a conexão inicial após reiniciar o serviço. O estado pode aparecer como `reconnecting`. Estados `logged_out`, `temporarily_banned`, `client_outdated`, `stream_replaced` e `connection_failed` indicam situações distintas: remova e crie novamente uma sessão despareada; aguarde a expiração de um banimento temporário; atualize a dependência whatsmeow para cliente desatualizado; verifique se outro processo está usando as mesmas credenciais quando houver substituição de stream. Cada conta segue sua própria reconexão.

### Contas que solicitam passkey

Quando o evento `session.passkey_required` for recebido, o cliente autorizado consulta `GET /v1/sessions/{id}/passkey`. O objeto `public_key` é o desafio recebido do whatsmeow. Um frontend deve convertê-lo para os tipos binários esperados pelo navegador, chamar `navigator.credentials.get()` em uma origem aceita pelo relying party do desafio — por exemplo, uma integração executada no contexto autorizado do WhatsApp Web — e enviar o resultado JSON do autenticador em `POST /v1/sessions/{id}/passkey`. Uma página comum servida em `localhost` não pode responder a um desafio de outro domínio. Esta API aceita campos extras emitidos pelo navegador e limita o corpo a 64 KiB.

Se o estado/evento indicar confirmação, consulte `GET /v1/sessions/{id}/passkey/confirmation`. Quando `confirmation_required` for `true`, compare visualmente o `code` com o exibido no WhatsApp do titular e, somente após essa conferência, envie `POST` para a mesma rota com `{"code":"...","confirm":true}`. Não automatize a confirmação sem a conferência do titular. Desafios, códigos e respostas WebAuthn são dados temporários e não devem ser registrados nem armazenados em cache. O servidor não fornece um frontend WebAuthn; portanto, o pareamento passkey exige integração de navegador e um autenticador disponível ao titular.

### Conectar duas ou mais contas

Depois de iniciar a API e definir `API_TOKEN`, rode este fluxo no PowerShell. Repita as chamadas de criação e conexão para cada conta, usando um ID diferente:

```powershell
$base = 'http://127.0.0.1:8080'
$headers = @{ Authorization = "Bearer $env:API_TOKEN" }

Invoke-RestMethod -Method Post -Uri "$base/v1/sessions" -Headers $headers -ContentType 'application/json' -Body '{"id":"conta-a"}'
Invoke-RestMethod -Method Post -Uri "$base/v1/sessions" -Headers $headers -ContentType 'application/json' -Body '{"id":"conta-b"}'

Invoke-RestMethod -Method Post -Uri "$base/v1/sessions/conta-a/connect" -Headers $headers
Invoke-RestMethod -Method Post -Uri "$base/v1/sessions/conta-b/connect" -Headers $headers

Invoke-WebRequest -Uri "$base/v1/sessions/conta-a/pairing?format=png" -Headers $headers -OutFile '.\conta-a-qr.png'
Invoke-WebRequest -Uri "$base/v1/sessions/conta-b/pairing?format=png" -Headers $headers -OutFile '.\conta-b-qr.png'
Start-Process '.\conta-a-qr.png'
Start-Process '.\conta-b-qr.png'
```

As duas imagens são distintas. Escaneie cada uma com a conta correspondente em **WhatsApp > Aparelhos conectados > Conectar aparelho**. Os QR temporários giram durante a tentativa de pareamento; se a imagem expirar antes de escanear, rode novamente o `Invoke-WebRequest` daquela sessão para baixar a imagem atual. Para confirmar, consulte `GET /v1/sessions`; o campo `account` passa a exibir o JID pareado e o estado deve ficar `connected`. O endpoint JSON sem `format=png` continua disponível para integrações que preferem desenhar o QR no próprio painel.

Após o pareamento, as chamadas continuam isoladas pelo ID:

```powershell
$body = @{ to = '5511999999999@s.whatsapp.net'; text = 'Mensagem pela conta A' } | ConvertTo-Json
Invoke-RestMethod -Method Post -Uri "$base/v1/sessions/conta-a/messages" -Headers $headers -ContentType 'application/json' -Body $body
```

Um exemplo para enviar texto:

```json
{"to":"5511999999999@s.whatsapp.net","text":"Olá"}
```

Para mídia, envie `multipart/form-data` com `to`, `file` e, opcionalmente, `caption`. Imagens, vídeos e áudios usam seus tipos nativos; outros arquivos são enviados como documento. No recebimento, imagens, vídeos, áudios, documentos e stickers entram na fila de download e no histórico; legendas são salvas com a mensagem. O tamanho padrão máximo é 16 MiB e pode ser configurado até 64 MiB. Anexos recebidos são persistidos em `whatsapp.api_media` no Supabase e anunciados por evento `message.media`, com URL autenticada para download. Mensagem e notificação de mídia são eventos separados.

O endpoint de preferências de chat recebe um patch como `{"archived":true,"pinned":false,"muted_until":"2026-10-02T12:00:00Z"}`. `muted_until` vazio remove o silenciamento. Essas opções ficam no armazenamento local da sessão e não prometem sincronização imediata com outros dispositivos.

As mensagens de texto aceitam até 65.536 bytes. JIDs precisam ser de usuário ou grupo. Requests JSON rejeitam campos desconhecidos. Erros seguem `{"error":{"code":"...","message":"..."}}`. Uma repetição do cliente após timeout de envio pode duplicar uma mensagem; ainda não há chave de idempotência para envios.

## Eventos e webhooks

Configure `WEBHOOK_URL` (HTTPS) e `WEBHOOK_SECRET` juntos. O envelope tem `id`, `type`, `session_id`, `timestamp` e `data`. `X-Webhook-Signature` contém `sha256=<hex HMAC-SHA256 do corpo bruto>` e `X-Event-ID` é estável durante retentativas. `WEBHOOK_TOKEN`, se definido, é enviado como header `X-Webhook-Token`. Destinos HTTP privados ficam desativados por padrão.

Eventos de ciclo de vida incluem `session.reconnecting`, `session.connected`, `session.logged_out`, `session.temporarily_banned`, `session.client_outdated`, `session.stream_replaced` e `session.connect_failed`.

Eventos são persistidos no Postgres privado do Supabase antes de serem encaminhados. Falhas recebem backoff progressivo até 12 tentativas; depois são removidas e registradas sem payload. Entrega é pelo menos uma vez: o consumidor deve deduplicar pelo ID. A fila pode crescer se o destino ficar indisponível, então monitore espaço em disco. A instância foi projetada para um processo único no Render, ligado ao Postgres do Supabase.

## Persistência e limitações

- Credenciais usam o `sqlstore` da whatsmeow. Cada sessão recebe um dispositivo próprio e as sessões pareadas são reabertas na inicialização.
- As chaves whatsmeow usam o sqlstore da whatsmeow no schema privado whatsapp do Postgres.
- Consultas de contatos são locais ao cache sincronizado; consulta de grupos exige sessão conectada.
- Histórico, mídia recebida, eventos, produtos e chaves WhatsApp são armazenados no schema privado do Supabase.
- O WhatsApp pode alterar o protocolo sem aviso. Esta API não promete disponibilidade, compatibilidade futura ou suporte oficial.
- A implementação não oferece mecanismos para spam, evasão de limites ou coleta não autorizada.

## Verificação executada

Com Go 1.26.8, `gofmt` foi aplicado, `go test ./...` passou, `go vet ./...` passou, a imagem Docker foi compilada e um smoke test confirmou readiness e autenticação. Os testes não precisam de uma conta WhatsApp real. Após os ajustes de DSN, ordem do rate limit, limpeza interna e a adição do painel web, o projeto foi reverificado com Go 1.27.0 (`gofmt`, `go vet`, `go test` e `go build`) e o ciclo completo foi exercitado em contêiner: painel carregado, token salvo, sessão criada e conectada até exibir o QR real, e remoção da sessão sem pareamento concluída (204).
