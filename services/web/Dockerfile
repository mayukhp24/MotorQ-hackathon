# Build context: services/web
FROM node:22-alpine AS build
WORKDIR /src
RUN --mount=type=secret,id=extra_ca,required=false \
    if [ -s /run/secrets/extra_ca ]; then cp /run/secrets/extra_ca /tmp/extra-ca.pem; fi
COPY package.json package-lock.json ./
RUN if [ -f /tmp/extra-ca.pem ]; then export NODE_EXTRA_CA_CERTS=/tmp/extra-ca.pem; fi; npm ci --no-audit --no-fund
COPY . .
RUN npm run build

FROM nginx:1.27-alpine
COPY nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /src/dist /usr/share/nginx/html
RUN chown -R nginx:nginx /var/cache/nginx /usr/share/nginx/html && \
    sed -i 's/^user .*;//' /etc/nginx/nginx.conf && \
    sed -i 's#pid .*;#pid /tmp/nginx.pid;#' /etc/nginx/nginx.conf && touch /tmp/nginx.pid && chown nginx /tmp/nginx.pid
USER nginx
EXPOSE 8080
