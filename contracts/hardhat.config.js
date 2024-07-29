require("@nomicfoundation/hardhat-toolbox");

module.exports = {
  solidity: "0.8.19",
  networks: {
    swisstronik: {
      url: "https://json-rpc.testnet.swisstronik.com/",
      accounts: 0xc48Bcd419C8B0274774178753fbe0dbAEA411DcA, //12c8e316294c2294a1e62b6c870619fc6a9134e6840c378c97b0d89884368a2d
    },
  },
};
