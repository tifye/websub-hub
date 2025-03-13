## Config
Default ports:
  - hub: 9999
  - client: 9998

Hub exposes two endpoints:
 - `POST /` for subscriptions
 - `POST /publish?topic={topic}` to publish hardcoded data to subscribers of the passed topic
   - topic will default to "a-topic" if none is passed 

