const { anyValue } = require("@nomicfoundation/hardhat-chai-matchers/withArgs");
const { expect } = require("chai");

describe('Contract tests', () => {
    describe('Counter', () => {
        let counterContract

        before(async () => {
            const Counter = await ethers.getContractFactory('Counter')
            counterContract = await Counter.deploy()
            await counterContract.deployed()
        })

        it('Should return code of deployed contract', async () => {
            const code = await ethers.provider.getCode(counterContract.address)
            expect(code).to.be.not.equal("0x")
        })

        // it('Should add', async () => {
        //     const countBefore = await counterContract.counter()
        //
        //     await expect(counterContract.add())
        //         .to.emit(counterContract, 'Added')
        //         .to.emit(counterContract, 'Changed')
        //
        //     const countAfter = await counterContract.counter()
        //     expect(countAfter).to.be.equal(countBefore + 1)
        // })
        //
        // it('Should subtract', async () => {
        //     const countBefore = await counterContract.counter()
        //
        //     await expect(counterContract.subtract())
        //         .to.emit(counterContract, 'Changed')
        //
        //     const countAfter = await counterContract.counter()
        //     expect(countAfter).to.be.equal(countBefore - 1)
        // })
        //
        // it('Should revert correctly', async () => {
        //     await expect(counterContract.subtract())
        //         .to.be.revertedWith("COUNTER_TOO_LOW")
        // })
    })
})